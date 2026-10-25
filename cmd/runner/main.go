// Command runner is the Cloud Run Job entrypoint: claim the run, mint a
// GitHub token, clone, run Claude Code with /build-board, report back.
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

	"github.com/nkirsch/auto-builder/internal/github"
)

const (
	agentTimeout = 55 * time.Minute
	summaryLen   = 4000
	maxLog       = 20 << 20
)

type config struct {
	RunID, BaseURL, Token, WorkDir string
	AppID, AppInstallation, AppKey string
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
		RunID:           env("RUN_ID", ""),
		BaseURL:         strings.TrimSuffix(env("BASE_URL", ""), "/"),
		Token:           env("RUNNER_TOKEN", ""),
		WorkDir:         env("WORK_DIR", os.TempDir()),
		AppID:           env("GH_APP_ID", ""),
		AppInstallation: env("GH_APP_INSTALLATION_ID", ""),
		AppKey:          env("GH_APP_PRIVATE_KEY", ""),
	}
	if cfg.RunID == "" || cfg.BaseURL == "" || cfg.Token == "" {
		slog.Error("RUN_ID, BASE_URL and RUNNER_TOKEN are required")
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
	if err := build(ctx, cfg, api, claim, log); err != nil {
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
	app, err := github.NewApp(cfg.AppID, cfg.AppInstallation, cfg.AppKey)
	if err != nil {
		return err
	}
	if app == nil {
		return errors.New("GH_APP_ID, GH_APP_INSTALLATION_ID and GH_APP_PRIVATE_KEY are required")
	}
	token, err := app.Token(ctx)
	if err != nil {
		return err
	}
	name, email, err := app.BotIdentity(ctx)
	if err != nil {
		return fmt.Errorf("bot identity: %w", err)
	}

	repoDir := filepath.Join(cfg.WorkDir, "repo")
	os.RemoveAll(repoDir)
	gitEnv := append(os.Environ(), "GH_TOKEN="+token, "GIT_TERMINAL_PROMPT=0")
	// gh serves git credentials from GH_TOKEN, so the token never lands in .git/config.
	steps := [][]string{
		{"gh", "auth", "setup-git"},
		{"git", "clone", "--depth", "50", "https://github.com/" + claim.Repo + ".git", repoDir},
		{"git", "-C", repoDir, "config", "user.name", name},
		{"git", "-C", repoDir, "config", "user.email", email},
	}
	for _, s := range steps {
		if err := runCmd(ctx, "", gitEnv, log, s...); err != nil {
			return err
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
	agentEnv := append(gitEnv, "BOARD_URL="+cfg.BaseURL, "RUN_ID="+cfg.RunID)
	err = runCmd(actx, repoDir, agentEnv, log, "claude", "-p", "/build-board",
		"--dangerously-skip-permissions", "--mcp-config", mcpConfig,
		"--output-format", "stream-json", "--verbose")
	if actx.Err() == context.DeadlineExceeded {
		return fmt.Errorf("agent timed out after %s", agentTimeout)
	}
	return err
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
	RunID    int    `json:"run_id"`
	Repo     string `json:"repo"`
	BotLogin string `json:"bot_login"`
}

func (a *api) claim(ctx context.Context, id string) (claimResp, error) {
	var out claimResp
	err := a.post(ctx, "/api/runner/runs/"+id+"/claim", nil, &out)
	return out, err
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
