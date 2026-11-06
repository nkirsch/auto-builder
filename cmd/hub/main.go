// Command hub drives auto-builder hubs from a terminal through their tenant
// API (bearer ADMIN_TOKEN):
//
//	hub [-json|-csv] [-hub NAME,...]   boards on every hub, like /admin/hubs
//	hub add [-run] [-draft] BOARD TITLE [BODY]
//	hub run BOARD                      start BOARD's run now
//
// BOARD is a tenant slug, found on whichever hub has it. Hubs come from
// $HUBS_CONFIG or ~/.config/hub/hubs, one per line: "URL PROJECT SECRET", the
// hub and the Secret Manager secret holding its ADMIN_TOKEN, or "URL
// env:VAR" for a token in $VAR. -hub keeps only hubs whose URL or name contains
// one of its comma-separated NAMEs, for every command.
package main

import (
	"bytes"
	"cmp"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"text/tabwriter"
	"time"
)

func main() {
	if err := run(context.Background(), os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "hub:", err)
		os.Exit(1)
	}
}

const usage = `usage:
  hub [-json|-csv]                          boards on every hub and what needs attention
  hub add [-run] [-draft] BOARD TITLE [BODY] file an approved ask (-draft: unapproved); -run starts a run
  hub run BOARD                             start BOARD's run now
  any of them: -hub NAME,...                only hubs whose URL or name contains a NAME`

func run(ctx context.Context, args []string, out io.Writer) error {
	cmd := "hubs"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	fs := flag.NewFlagSet("hub "+cmd, flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "print the raw JSON")
	asCSV := fs.Bool("csv", false, "print one row per board")
	only := fs.String("hub", "", "only hubs whose URL or name contains one of these comma-separated names")
	runNow := fs.Bool("run", false, "start a run after filing")
	draft := fs.Bool("draft", false, "leave the ask unapproved")
	pos, err := parseAnywhere(fs, args)
	if err != nil {
		return err
	}
	if *asJSON && *asCSV {
		return errors.New("-json or -csv, not both")
	}
	hubs, err := loadHubs()
	if err != nil {
		return err
	}
	if hubs, err = selectHubs(hubs, *only); err != nil {
		return err
	}
	c := &client{http: &http.Client{Timeout: 30 * time.Second}}
	switch {
	case cmd == "hubs" && len(pos) == 0:
		raw, v, err := c.overview(ctx, hubs, *only)
		if err != nil {
			return err
		}
		switch {
		case *asJSON:
			_, err := out.Write(raw)
			return err
		case *asCSV:
			return writeCSV(out, v)
		}
		printHubs(out, v, time.Now())
		return nil
	case cmd == "add" && (len(pos) == 2 || len(pos) == 3):
		h, err := c.find(ctx, hubs, pos[0])
		if err != nil {
			return err
		}
		in := map[string]any{"title": pos[1], "approve": !*draft}
		if len(pos) == 3 {
			in["body"] = pos[2]
		}
		var it struct {
			ID       int    `json:"id"`
			URL      string `json:"url"`
			Approved bool   `json:"approved"`
		}
		if _, err := c.do(ctx, h, "POST", "/api/tenants/"+pos[0]+"/items", in, &it); err != nil {
			return err
		}
		state := "approved"
		if !it.Approved {
			state = "unapproved"
		}
		fmt.Fprintf(out, "#%d %s (%s)\n", it.ID, it.URL, state)
		if !*runNow {
			return nil
		}
		return c.start(ctx, out, h, pos[0])
	case cmd == "run" && len(pos) == 1:
		h, err := c.find(ctx, hubs, pos[0])
		if err != nil {
			return err
		}
		return c.start(ctx, out, h, pos[0])
	}
	return errors.New(usage)
}

// parseAnywhere parses flags before, between and after positional args.
func parseAnywhere(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		if fs.NArg() == 0 {
			return pos, nil
		}
		pos, args = append(pos, fs.Arg(0)), fs.Args()[1:]
	}
}

// hub is one hub and where its ADMIN_TOKEN comes from.
type hub struct {
	URL, Project, Secret, Env string
}

func loadHubs() ([]hub, error) {
	text := os.Getenv("HUBS_CONFIG")
	if text == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		b, err := os.ReadFile(filepath.Join(home, ".config", "hub", "hubs"))
		if err != nil {
			return nil, fmt.Errorf("no hubs: set HUBS_CONFIG or write ~/.config/hub/hubs (%w)", err)
		}
		text = string(b)
	}
	return parseHubs(text)
}

func parseHubs(text string) ([]hub, error) {
	var hubs []hub
	for _, line := range strings.Split(text, "\n") {
		line, _, _ = strings.Cut(line, "#")
		f := strings.Fields(line)
		switch {
		case len(f) == 0:
			continue
		case len(f) == 2 && strings.HasPrefix(f[1], "env:"):
			hubs = append(hubs, hub{URL: strings.TrimSuffix(f[0], "/"), Env: strings.TrimPrefix(f[1], "env:")})
		case len(f) == 3:
			hubs = append(hubs, hub{URL: strings.TrimSuffix(f[0], "/"), Project: f[1], Secret: f[2]})
		default:
			return nil, fmt.Errorf("bad hubs line %q: want URL PROJECT SECRET or URL env:VAR", line)
		}
	}
	if len(hubs) == 0 {
		return nil, errors.New("no hubs listed")
	}
	return hubs, nil
}

// matches reports whether any of name's values contains one of the comma-separated terms.
func matches(only string, values ...string) bool {
	for _, term := range strings.Split(strings.ToLower(only), ",") {
		for _, v := range values {
			if term = strings.TrimSpace(term); term != "" && strings.Contains(strings.ToLower(v), term) {
				return true
			}
		}
	}
	return false
}

// selectHubs narrows hubs to those matching only, when it's set.
func selectHubs(hubs []hub, only string) ([]hub, error) {
	if only == "" {
		return hubs, nil
	}
	var kept []hub
	var urls []string
	for _, h := range hubs {
		urls = append(urls, h.URL)
		if matches(only, h.URL) {
			kept = append(kept, h)
		}
	}
	if len(kept) == 0 {
		return nil, fmt.Errorf("no hub matches %q in %s", only, strings.Join(urls, ", "))
	}
	return kept, nil
}

type client struct {
	http   *http.Client
	mu     sync.Mutex
	tokens map[string]string
}

// token is the hub's ADMIN_TOKEN, from its env var or Secret Manager.
func (c *client) token(ctx context.Context, h hub) (string, error) {
	if h.Env != "" {
		if t := os.Getenv(h.Env); t != "" {
			return t, nil
		}
		return "", fmt.Errorf("%s: $%s is empty", h.URL, h.Env)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if t, ok := c.tokens[h.URL]; ok {
		return t, nil
	}
	b, err := exec.CommandContext(ctx, "gcloud", "secrets", "versions", "access", "latest",
		"--secret", h.Secret, "--project", h.Project).Output()
	if ee := (*exec.ExitError)(nil); errors.As(err, &ee) && len(ee.Stderr) > 0 {
		return "", fmt.Errorf("%s: reading %s/%s: %s", h.URL, h.Project, h.Secret, strings.TrimSpace(string(ee.Stderr)))
	} else if err != nil {
		return "", fmt.Errorf("%s: reading %s/%s: %w", h.URL, h.Project, h.Secret, err)
	}
	if c.tokens == nil {
		c.tokens = map[string]string{}
	}
	c.tokens[h.URL] = strings.TrimSpace(string(b))
	return c.tokens[h.URL], nil
}

// apiError is an API answer other than 2xx.
type apiError struct {
	Code int
	Msg  string
}

func (e *apiError) Error() string { return fmt.Sprintf("HTTP %d: %s", e.Code, e.Msg) }

// do sends in as JSON and decodes the answer into out; it returns the raw body.
func (c *client) do(ctx context.Context, h hub, method, path string, in, out any) ([]byte, error) {
	tok, err := c.token(ctx, h)
	if err != nil {
		return nil, err
	}
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, h.URL+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(raw, &e) != nil || e.Error == "" {
			e.Error = strings.TrimSpace(string(raw))
		}
		return raw, &apiError{resp.StatusCode, e.Error}
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return raw, fmt.Errorf("%s%s: %w", h.URL, path, err)
		}
	}
	return raw, nil
}

// find is the one hub with the board slug.
func (c *client) find(ctx context.Context, hubs []hub, slug string) (hub, error) {
	if slug == "" || strings.ContainsAny(slug, "/?#") {
		return hub{}, fmt.Errorf("bad board %q", slug)
	}
	errs := make([]error, len(hubs))
	var wg sync.WaitGroup
	for i, h := range hubs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = c.do(ctx, h, "GET", "/api/tenants/"+slug, nil, nil)
		}()
	}
	wg.Wait()
	var found []hub
	var failed []string
	for i, err := range errs {
		var ae *apiError
		switch {
		case err == nil:
			found = append(found, hubs[i])
		case errors.As(err, &ae) && ae.Code == http.StatusNotFound:
		default:
			failed = append(failed, fmt.Sprintf("%s: %v", hubs[i].URL, err))
		}
	}
	switch {
	case len(found) == 1:
		return found[0], nil
	case len(found) > 1:
		return hub{}, fmt.Errorf("board %s is on more than one hub", slug)
	case len(failed) > 0:
		return hub{}, fmt.Errorf("board %s not found; %s", slug, strings.Join(failed, "; "))
	}
	return hub{}, fmt.Errorf("no hub has a board %s", slug)
}

// start starts the board's run now.
func (c *client) start(ctx context.Context, out io.Writer, h hub, slug string) error {
	var r struct {
		Run  int    `json:"run"`
		Runs string `json:"runs"`
	}
	if _, err := c.do(ctx, h, "POST", "/api/tenants/"+slug+"/run", nil, &r); err != nil {
		var ae *apiError
		if errors.As(err, &ae) && ae.Code == http.StatusConflict {
			return fmt.Errorf("not started: %s", ae.Msg)
		}
		return err
	}
	fmt.Fprintf(out, "run %d started: %s\n", r.Run, r.Runs)
	return nil
}

// overview merges every hub's GET /api/hubs, in config order. A hub that can't
// be read becomes a hub with an Error, so the others still show. Hubs can list
// each other: each appears once, preferring an answer without an error, and
// when only is set peers it doesn't match are dropped.
func (c *client) overview(ctx context.Context, hubs []hub, only string) ([]byte, hubsView, error) {
	type answer struct {
		Hubs      []json.RawMessage `json:"hubs"`
		Attention []json.RawMessage `json:"attention"`
	}
	answers := make([]answer, len(hubs))
	errs := make([]error, len(hubs))
	var wg sync.WaitGroup
	for i, h := range hubs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = c.do(ctx, h, "GET", "/api/hubs", nil, &answers[i])
		}()
	}
	wg.Wait()
	if !slices.ContainsFunc(errs, func(err error) bool { return err == nil }) {
		return nil, hubsView{}, errors.Join(errs...)
	}

	type entry struct {
		Name  string `json:"name"`
		URL   string `json:"url"`
		Error string `json:"error"`
	}
	var merged []json.RawMessage
	at := map[string]int{}      // hub URL → index in merged
	failed := map[string]bool{} // hub URLs whose kept entry has an error
	names := map[string]bool{}  // names of kept hubs, which attention refers to
	for i, a := range answers {
		if errs[i] != nil {
			b, err := json.Marshal(entry{Name: strings.TrimPrefix(hubs[i].URL, "https://"), URL: hubs[i].URL, Error: errs[i].Error()})
			if err != nil {
				return nil, hubsView{}, err
			}
			a.Hubs = []json.RawMessage{b}
		}
		for _, raw := range a.Hubs {
			var e entry
			if err := json.Unmarshal(raw, &e); err != nil {
				return nil, hubsView{}, fmt.Errorf("%s/api/hubs: %w", hubs[i].URL, err)
			}
			if only != "" && !matches(only, e.URL, e.Name) {
				continue
			}
			names[e.Name] = true
			j, seen := at[e.URL]
			switch {
			case !seen:
				at[e.URL] = len(merged)
				merged = append(merged, raw)
			case failed[e.URL] && e.Error == "":
				merged[j] = raw
			default:
				continue
			}
			failed[e.URL] = e.Error != ""
		}
	}

	var attention []json.RawMessage
	seen := map[string]bool{}
	for _, a := range answers {
		for _, raw := range a.Attention {
			var item struct{ Hub, Title, URL string }
			if err := json.Unmarshal(raw, &item); err != nil {
				return nil, hubsView{}, err
			}
			key := item.URL + "\x00" + item.Title
			if seen[key] || !names[item.Hub] {
				continue
			}
			seen[key] = true
			attention = append(attention, raw)
		}
	}

	raw, err := json.Marshal(answer{merged, attention})
	if err != nil {
		return nil, hubsView{}, err
	}
	var v hubsView
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, hubsView{}, err
	}
	// Each hub's own total can't be summed when hubs list each other, but the boards' can.
	for _, h := range v.Hubs {
		slices.SortStableFunc(h.Sites, func(a, b hubSite) int {
			return strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name))
		})
		for _, s := range h.Sites {
			v.Other += s.Stats.Other
		}
	}
	raw, err = json.Marshal(struct {
		answer
		Other int `json:"other"`
	}{answer{merged, attention}, v.Other})
	return raw, v, err
}

// activity is the newest thing to happen on a board.
type activity struct {
	Kind string    `json:"kind"`
	At   time.Time `json:"at"`
}

// hubSite is one board in GET /api/hubs.
type hubSite struct {
	Name        string `json:"name"`
	Site        string `json:"site"`
	App         bool   `json:"app"`
	Health      string `json:"health"`
	HealthErr   string `json:"health_err"`
	Detail      bool   `json:"detail"`
	Releases    bool   `json:"releases"`
	Unreleased  int    `json:"unreleased"`
	Errors24h   int    `json:"errors_24h"`
	ErrorsKnown bool   `json:"errors_known"`
	Stats       struct {
		Version   string            `json:"version"`
		GitSHA    string            `json:"sha"`
		Attention []json.RawMessage `json:"attention"`
		Other     int               `json:"other"`
		Failed7d  int               `json:"failed_7d"`
		LastRun   *struct {
			Status string    `json:"status"`
			At     time.Time `json:"at"`
		} `json:"last_run"`
		LastActivity *activity `json:"last_activity"`
		LastHuman    *activity `json:"last_human_activity"`
	} `json:"stats"`
}

// writeCSV writes one row per board; a hub that couldn't be read is one row with its error.
func writeCSV(w io.Writer, v hubsView) error {
	cw := csv.NewWriter(w)
	cw.Write([]string{"hub", "board", "health", "health_error", "version", "standalone", "unreleased",
		"errors_24h", "attention", "other", "last_run", "last_run_at", "failed_7d", "site", "hub_url", "hub_error", "last_activity", "last_activity_at", "last_human", "last_human_at"})
	for _, h := range v.Hubs {
		if h.Error != "" {
			cw.Write([]string{h.Name, "", "", "", "", "", "", "", "", "", "", "", "", "", h.URL, h.Error, "", "", "", ""})
		}
		for _, s := range h.Sites {
			// Blank means unknown, so a count of 0 stays distinct.
			unreleased, errs, attn, other, last, at := "", "", "", "", "", ""
			if s.Releases {
				unreleased = fmt.Sprint(s.Unreleased)
			}
			if s.ErrorsKnown {
				errs = fmt.Sprint(s.Errors24h)
			}
			if s.Detail {
				attn, other = fmt.Sprint(len(s.Stats.Attention)), fmt.Sprint(s.Stats.Other)
			}
			if r := s.Stats.LastRun; r != nil {
				last, at = r.Status, r.At.Format(time.RFC3339)
			}
			cells := func(a *activity) (string, string) {
				if a == nil {
					return "", ""
				}
				return a.Kind, a.At.Format(time.RFC3339)
			}
			kind, at := cells(s.Stats.LastActivity)
			human, humanAt := cells(s.Stats.LastHuman)
			cw.Write([]string{h.Name, s.Name, s.Health, s.HealthErr, cmp.Or(s.Stats.Version, short(s.Stats.GitSHA)),
				fmt.Sprint(!s.App), unreleased, errs, attn, other, last, at, fmt.Sprint(s.Stats.Failed7d), s.Site, h.URL, "", kind, at, human, humanAt})
		}
	}
	cw.Flush()
	return cw.Error()
}

// hubsView is GET /api/hubs.
type hubsView struct {
	Hubs []struct {
		Name         string    `json:"name"`
		URL          string    `json:"url"`
		Version      string    `json:"version"`
		Error        string    `json:"error"`
		Upstream     string    `json:"upstream"`
		Behind       int       `json:"behind"`
		More         bool      `json:"behind_more"`
		Unreleased   int       `json:"unreleased"`
		ReleaseKnown bool      `json:"release_known"`
		Sites        []hubSite `json:"sites"`
	} `json:"hubs"`
	Attention []struct {
		ID     int       `json:"id"`
		Title  string    `json:"title"`
		Author string    `json:"author"`
		Kind   string    `json:"kind"`
		Note   string    `json:"note"`
		At     time.Time `json:"at"`
		Hub    string    `json:"hub"`
		Site   string    `json:"site"`
		URL    string    `json:"url"`
	} `json:"attention"`
	Other int `json:"other"`
}

// printHubs writes v as /admin/hubs lays it out: attention, then each hub's boards.
func printHubs(w io.Writer, v hubsView, now time.Time) {
	fmt.Fprintln(w, "NEEDS ATTENTION")
	if len(v.Attention) == 0 {
		fmt.Fprintln(w, "  nothing urgent, escalated, blocked or failing")
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, a := range v.Attention {
		where := a.Hub + " · " + a.Site
		if a.ID != 0 {
			where += fmt.Sprintf(" · #%d by %s", a.ID, a.Author)
		}
		if !a.At.IsZero() {
			where += " · " + ago(now, a.At)
		}
		line := fmt.Sprintf("  %s\t%s\t%s\t%s", a.Kind, a.Title, where, a.URL)
		if a.Note != "" {
			line += "\n  \t— " + a.Note
		}
		fmt.Fprintln(tw, line)
	}
	tw.Flush()
	fmt.Fprintf(w, "  %d other unfinished item%s\n", v.Other, plural(v.Other))
	for _, h := range v.Hubs {
		head := h.Name
		if h.Version != "" {
			head += " " + h.Version
		}
		if h.Error == "" {
			switch {
			case !h.ReleaseKnown:
			case h.Unreleased > 0:
				head += fmt.Sprintf(" · %d unreleased", h.Unreleased)
			default:
				head += " · released"
			}
			if h.Upstream != "" && h.Behind > 0 {
				more := ""
				if h.More {
					more = "+"
				}
				head += fmt.Sprintf(" · %d%s behind %s", h.Behind, more, h.Upstream)
			}
		}
		fmt.Fprintf(w, "\n── %s  %s\n", strings.ToUpper(head), h.URL)
		if h.Error != "" {
			fmt.Fprintf(w, "  couldn't read %s/api/overview: %s\n", h.URL, h.Error)
			continue
		}
		if len(h.Sites) == 0 {
			fmt.Fprintln(w, "  no boards")
			continue
		}
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "  BOARD\tHEALTH\tVERSION\t5XX\tATTN\tOTHER\tLAST RUN\tLAST ACTIVITY\tLAST HUMAN")
		for _, s := range h.Sites {
			name := s.Name
			if !s.App {
				name += " (standalone)"
			}
			health := s.Health
			if s.HealthErr != "" && health != "ok" && health != "ready" {
				health += ": " + s.HealthErr
			}
			errs := "–"
			if s.ErrorsKnown {
				errs = fmt.Sprint(s.Errors24h)
			}
			attn, other := "–", "–"
			if s.Detail {
				attn, other = fmt.Sprint(len(s.Stats.Attention)), fmt.Sprint(s.Stats.Other)
			}
			last := "–"
			if r := s.Stats.LastRun; r != nil {
				last = r.Status + " " + ago(now, r.At)
			}
			if s.Stats.Failed7d > 0 {
				last += fmt.Sprintf(" · %d failed in 7d", s.Stats.Failed7d)
			}
			cell := func(a *activity) string {
				if a == nil {
					return "–"
				}
				return a.Kind + " " + ago(now, a.At)
			}
			fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", name, health, versionCell(s), errs, attn, other, last,
				cell(s.Stats.LastActivity), cell(s.Stats.LastHuman))
		}
		tw.Flush()
	}
}

// describeSuffix is what git describe adds to a tag: commits since it and the commit.
var describeSuffix = regexp.MustCompile(`-\d+-g[0-9a-f]+$`)

// versionCell is the version, with +N for N commits since the last release. That
// count replaces git describe's suffix; a suffix with no count stays visible.
func versionCell(s hubSite) string {
	v := cmp.Or(s.Stats.Version, short(s.Stats.GitSHA), "–")
	if s.Releases && s.Unreleased > 0 {
		return fmt.Sprintf("%s +%d", describeSuffix.ReplaceAllString(v, ""), s.Unreleased)
	}
	return v
}

func ago(now, t time.Time) string {
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return fmt.Sprintf("%dd ago", int(d.Hours()/24))
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
