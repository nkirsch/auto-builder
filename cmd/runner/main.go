// Command runner is the Cloud Run Job entrypoint: claim the run, get a
// GitHub token from the server, clone, run Claude Code with /build-board,
// report back. It never holds the GitHub App's private key.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	autobuilder "github.com/nkirsch/auto-builder"
	"github.com/nkirsch/auto-builder/internal/upstream"
)

const (
	agentTimeout = 55 * time.Minute
	summaryLen   = 4000
	maxLog       = 20 << 20
)

type config struct {
	RunID, BaseURL, Token, WorkDir string
}

func env(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" && v != "unset" {
		return v
	}
	return def
}

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))
	cfg := config{
		RunID:   env("RUN_ID", ""),
		BaseURL: strings.TrimSuffix(env("BASE_URL", ""), "/"),
		// The run's own token, valid only while the run is active.
		Token:   env("RUN_TOKEN", ""),
		WorkDir: env("WORK_DIR", os.TempDir()),
	}
	if cfg.RunID == "" || cfg.BaseURL == "" || cfg.Token == "" {
		slog.Error("RUN_ID, BASE_URL and RUN_TOKEN are required")
		os.Exit(2)
	}
	api := &api{base: cfg.BaseURL, token: cfg.Token, client: &http.Client{Timeout: 2 * time.Minute}}

	ctx := context.Background()
	claim, err := api.claim(ctx, cfg.RunID)
	if err != nil {
		slog.Error("claim", "run", cfg.RunID, "err", err)
		os.Exit(1)
	}
	slog.Info("claimed", "run", cfg.RunID, "repo", claim.Repo)

	log := &capped{max: maxLog}
	status := "done"
	work := build
	if claim.Upstream != "" {
		work = mergeUpstream
	}
	if err := work(ctx, cfg, api, claim, log); err != nil {
		status = "failed"
		fmt.Fprintf(log, "\nrunner: %v\n", err)
		slog.Error("build", "err", err)
	}
	api.stage(ctx, cfg.RunID, "report")
	text, cost := result(log.Bytes())
	if err := api.finish(ctx, cfg.RunID, status, text, log.String(), cost); err != nil {
		slog.Error("finish", "err", err)
		os.Exit(1)
	}
	slog.Info("finished", "run", cfg.RunID, "status", status)
	if status != "done" {
		os.Exit(1)
	}
}

func build(ctx context.Context, cfg config, api *api, claim claimResp, log io.Writer) error {
	gh, err := api.githubToken(ctx, cfg.RunID)
	if err != nil {
		return fmt.Errorf("github token: %w", err)
	}

	repoDir := filepath.Join(cfg.WorkDir, "repo")
	os.RemoveAll(repoDir)
	gitEnv := append(childEnv(os.Environ()), "GH_TOKEN="+gh.Token, "GIT_TERMINAL_PROMPT=0")
	// gh serves git credentials from GH_TOKEN, so the token never lands in .git/config.
	steps := [][]string{
		{"gh", "auth", "setup-git"},
		{"git", "clone", "--depth", "50", "https://github.com/" + claim.Repo + ".git", repoDir},
		{"git", "-C", repoDir, "config", "user.name", gh.Name},
		{"git", "-C", repoDir, "config", "user.email", gh.Email},
	}
	for _, s := range steps {
		if err := runCmd(ctx, "", gitEnv, log, s...); err != nil {
			return err
		}
	}

	if home, err := os.UserHomeDir(); err == nil {
		if installed, err := installCommand(repoDir, home); err != nil {
			return fmt.Errorf("install /build-board: %w", err)
		} else if installed {
			fmt.Fprintln(log, "runner: repo has no /build-board; using auto-builder's")
		}
	}

	mcpConfig := filepath.Join(cfg.WorkDir, "mcp.json")
	b, _ := json.Marshal(map[string]any{"mcpServers": map[string]any{"board": map[string]any{
		"type": "http", "url": cfg.BaseURL + "/mcp",
		"headers": map[string]string{"Authorization": "Bearer " + cfg.Token},
	}}})
	if err := os.WriteFile(mcpConfig, b, 0o600); err != nil {
		return err
	}

	api.stage(ctx, cfg.RunID, "agent")
	actx, cancel := context.WithTimeout(ctx, agentTimeout)
	defer cancel()
	agentEnv := append(gitEnv, "BOARD_URL="+cfg.BaseURL, "RUN_ID="+cfg.RunID, "AGENT_WORKFLOWS="+onOff(claim.Workflows))
	if fed, err := claudeIdentity(actx, cfg.WorkDir, http.DefaultClient); err != nil {
		return fmt.Errorf("claude identity token: %w", err)
	} else if fed != nil {
		agentEnv = append(withoutAPIKey(agentEnv), fed...)
	}
	err = runCmd(actx, repoDir, agentEnv, log, "claude", "-p", "/build-board",
		"--dangerously-skip-permissions", "--mcp-config", mcpConfig,
		"--output-format", "stream-json", "--verbose")
	if actx.Err() == context.DeadlineExceeded {
		return fmt.Errorf("agent timed out after %s", agentTimeout)
	}
	return err
}

// mergeUpstream merges claim.Upstream into an upstream-<sha> branch of the
// repo; the server opens the PR and lands it on green CI.
func mergeUpstream(ctx context.Context, cfg config, api *api, claim claimResp, log io.Writer) error {
	gh, err := api.githubToken(ctx, cfg.RunID)
	if err != nil {
		return fmt.Errorf("github token: %w", err)
	}
	env := append(childEnv(os.Environ()), "GH_TOKEN="+gh.Token, "GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME="+gh.Name, "GIT_AUTHOR_EMAIL="+gh.Email,
		"GIT_COMMITTER_NAME="+gh.Name, "GIT_COMMITTER_EMAIL="+gh.Email)
	// gh serves git credentials from GH_TOKEN, so the token never lands in .git/config.
	if err := runCmd(ctx, "", env, log, "gh", "auth", "setup-git"); err != nil {
		return err
	}
	dir := filepath.Join(cfg.WorkDir, "repo")
	os.RemoveAll(dir)
	branch, err := upstream.Merge(ctx, logShell{log}, env, dir, claim.Repo, claim.Upstream, "master")
	if err != nil {
		return err
	}
	if branch == "" {
		fmt.Fprintf(log, `{"type":"result","result":"Already up to date with %s."}`+"\n", claim.Upstream)
	} else {
		fmt.Fprintf(log, `{"type":"result","result":"Merged %s into branch %s; the board opens the PR and lands it once CI passes."}`+"\n", claim.Upstream, branch)
	}
	return nil
}

// logShell runs commands for upstream.Merge, echoing them and their output to the run log.
type logShell struct{ log io.Writer }

func (l logShell) Run(ctx context.Context, env []string, args ...string) (string, error) {
	fmt.Fprintf(l.log, "$ %s\n", strings.Join(args, " "))
	var out bytes.Buffer
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Env = env
	cmd.Stdout = io.MultiWriter(&out, l.log)
	cmd.Stderr = l.log
	err := cmd.Run()
	return out.String(), err
}

// installCommand puts auto-builder's /build-board in the user's commands when
// the repo doesn't carry its own (a tenant repo), reporting whether it did.
func installCommand(repoDir, home string) (bool, error) {
	if _, err := os.Stat(filepath.Join(repoDir, ".claude", "commands", "build-board.md")); err == nil {
		return false, nil
	}
	dir := filepath.Join(home, ".claude", "commands")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false, err
	}
	return true, os.WriteFile(filepath.Join(dir, "build-board.md"), []byte(autobuilder.BuildBoard), 0o644)
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

// childEnv drops GH_APP_* and RUNNER_TOKEN (a job still mounting the old
// secrets) so neither git nor the agent sees a long-lived credential.
func childEnv(environ []string) []string {
	var out []string
	for _, kv := range environ {
		if !strings.HasPrefix(kv, "GH_APP_") && !strings.HasPrefix(kv, "RUNNER_TOKEN=") {
			out = append(out, kv)
		}
	}
	return out
}

// metadataIdentityURL mints a Google-signed ID token for the job's service
// account; format=full adds the email claim the federation rule matches.
var metadataIdentityURL = "http://metadata.google.internal/computeMetadata/v1/instance/service-accounts/default/identity?audience=https://api.anthropic.com&format=full"

// identityRefresh keeps the token file ahead of Claude Code's re-exchanges:
// Google ID tokens last 1h and the agent may run 55 min.
const identityRefresh = 20 * time.Minute

// claudeIdentity sets up Workload Identity Federation for Claude Code when
// ANTHROPIC_FEDERATION_RULE_ID is set: it writes the job's Google ID token to
// a file, refreshes it until ctx ends, and returns the env pointing Claude
// Code at it. Nil means no federation; the agent uses ANTHROPIC_API_KEY.
func claudeIdentity(ctx context.Context, dir string, client *http.Client) ([]string, error) {
	if env("ANTHROPIC_FEDERATION_RULE_ID", "") == "" {
		return nil, nil
	}
	path := filepath.Join(dir, "anthropic-identity-token")
	write := func() error {
		req, _ := http.NewRequestWithContext(ctx, "GET", metadataIdentityURL, nil)
		req.Header.Set("Metadata-Flavor", "Google")
		resp, err := client.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		if resp.StatusCode != http.StatusOK || len(b) == 0 {
			return fmt.Errorf("metadata identity: HTTP %d", resp.StatusCode)
		}
		tmp := path + ".tmp"
		if err := os.WriteFile(tmp, b, 0o600); err != nil {
			return err
		}
		return os.Rename(tmp, path)
	}
	if err := write(); err != nil {
		return nil, err
	}
	go func() {
		t := time.NewTicker(identityRefresh)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := write(); err != nil {
					slog.Warn("refresh claude identity token", "err", err)
				}
			}
		}
	}()
	return []string{"ANTHROPIC_IDENTITY_TOKEN_FILE=" + path}, nil
}

// withoutAPIKey drops ANTHROPIC_API_KEY, which outranks federation in Claude
// Code's credential order (a job may still mount the secret, even as "unset").
func withoutAPIKey(environ []string) []string {
	var out []string
	for _, kv := range environ {
		if !strings.HasPrefix(kv, "ANTHROPIC_API_KEY=") {
			out = append(out, kv)
		}
	}
	return out
}

func runCmd(ctx context.Context, dir string, env []string, log io.Writer, args ...string) error {
	fmt.Fprintf(log, "$ %s\n", strings.Join(args, " "))
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Dir, cmd.Env = dir, env
	cmd.Stdout, cmd.Stderr = io.MultiWriter(log, os.Stdout), io.MultiWriter(log, os.Stderr)
	cmd.WaitDelay = 10 * time.Second
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %w", args[0], err)
	}
	return nil
}

// result reads the agent's final result event: the summary prefers its
// message, else the log's tail; cost is its total_cost_usd, nil if absent.
func result(log []byte) (summary string, cost *float64) {
	var msg string
	sc := bufio.NewScanner(bytes.NewReader(log))
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		var ev struct {
			Type    string   `json:"type"`
			Result  string   `json:"result"`
			CostUSD *float64 `json:"total_cost_usd"`
		}
		if json.Unmarshal(sc.Bytes(), &ev) != nil || ev.Type != "result" {
			continue
		}
		if ev.Result != "" {
			msg = ev.Result
		}
		if ev.CostUSD != nil {
			cost = ev.CostUSD
		}
	}
	summary = string(log)
	if msg != "" {
		summary = msg
	}
	if len(summary) > summaryLen {
		summary = summary[len(summary)-summaryLen:]
	}
	return summary, cost
}

// capped is a buffer that keeps the first max bytes.
type capped struct {
	bytes.Buffer
	max int
}

func (c *capped) Write(p []byte) (int, error) {
	if room := c.max - c.Len(); room > 0 {
		c.Buffer.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

type api struct {
	base, token string
	client      *http.Client
}

type claimResp struct {
	RunID     int    `json:"run_id"`
	Repo      string `json:"repo"`
	BotLogin  string `json:"bot_login"`
	Upstream  string `json:"upstream"`  // set for an upstream run: merge it, no agent
	Workflows bool   `json:"workflows"` // the agent's token may change workflows
}

func (a *api) claim(ctx context.Context, id string) (claimResp, error) {
	var out claimResp
	err := a.post(ctx, "/api/runner/runs/"+id+"/claim", nil, &out)
	return out, err
}

type tokenResp struct {
	Token string `json:"token"`
	Name  string `json:"name"`
	Email string `json:"email"`
}

// githubToken asks the server for a 1 h installation token and the bot's commit
// identity; the server hands out one per run.
func (a *api) githubToken(ctx context.Context, id string) (tokenResp, error) {
	var out tokenResp
	if err := a.post(ctx, "/api/runner/runs/"+id+"/token", nil, &out); err != nil {
		return out, err
	}
	if out.Token == "" || out.Name == "" || out.Email == "" {
		return out, errors.New("incomplete token response")
	}
	return out, nil
}

// stage tells the server the run reached a stage; it's only for display, so
// failures are logged and ignored.
func (a *api) stage(ctx context.Context, id, stage string) {
	if err := a.post(ctx, "/api/runner/runs/"+id+"/stage", map[string]string{"stage": stage}, nil); err != nil {
		slog.Warn("stage", "stage", stage, "err", err)
	}
}

func (a *api) finish(ctx context.Context, id, status, summary, log string, cost *float64) error {
	body := map[string]any{"status": status, "summary": summary, "log": log}
	if cost != nil {
		body["cost_usd"] = *cost
	}
	var err error
	for attempt := range 3 {
		if err = a.post(ctx, "/api/runner/runs/"+id+"/finish", body, nil); err == nil {
			return nil
		}
		time.Sleep(time.Duration(attempt+1) * 5 * time.Second)
	}
	return err
}

func (a *api) post(ctx context.Context, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.base+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+a.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("%s: %s %s", path, resp.Status, strings.TrimSpace(string(b)))
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}
