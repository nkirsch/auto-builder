package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nkirsch/auto-builder/internal/db"
)

// started approximates the deploy time: Cloud Run starts a fresh instance per revision.
var started = time.Now().UTC()

// siteStats is one board's summary for the hub's all-sites page; a single
// board serves it at GET /api/stats behind STATS_TOKEN.
type siteStats struct {
	Version  string    `json:"version"`
	GitSHA   string    `json:"sha"`
	Started  time.Time `json:"started"`
	Open     int       `json:"open"`
	LastRun  *runBrief `json:"last_run,omitempty"`
	Spend30d float64   `json:"spend_30d_usd"`
}

type runBrief struct {
	Status string    `json:"status"`
	At     time.Time `json:"at"` // finished, else created
}

func statsFor(ctx context.Context, st *db.Store, version, sha string) (siteStats, error) {
	s := siteStats{Version: version, GitSHA: sha, Started: started}
	items, err := st.Items(ctx, "open")
	if err != nil {
		return s, err
	}
	s.Open = len(items)
	runs, err := st.Runs(ctx, 1)
	if err != nil {
		return s, err
	}
	if len(runs) > 0 {
		r := runs[0]
		at := r.CreatedAt
		if r.FinishedAt != nil {
			at = *r.FinishedAt
		}
		s.LastRun = &runBrief{Status: r.Status, At: at}
	}
	c, err := st.RunCosts(ctx)
	s.Spend30d = c.Last30Days
	return s, err
}

func (s *server) stats(w http.ResponseWriter, r *http.Request) {
	st, err := statsFor(r.Context(), s.store, s.version(), s.cfg.GitSHA)
	if err != nil {
		slog.Error("stats", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// remoteSite is a single-board site from SITES ("name=board-URL ...", space- or comma-separated).
type remoteSite struct{ Name, URL string }

func parseSites(v string) ([]remoteSite, error) {
	var out []remoteSite
	for _, kv := range strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' }) {
		name, u, ok := strings.Cut(kv, "=")
		if !ok || name == "" || !strings.HasPrefix(u, "https://") && !strings.HasPrefix(u, "http://") {
			return nil, fmt.Errorf("SITES entry %q: want name=https://board", kv)
		}
		out = append(out, remoteSite{Name: strings.TrimSpace(name), URL: strings.TrimSuffix(u, "/")})
	}
	return out, nil
}

// siteRow is one line of the all-sites page.
type siteRow struct {
	Name, Board string
	App         bool   // a hub app, read from its store; else a remote single board
	Health      string // "ok", or why not
	Stats       siteStats
	Detail      bool // Stats beyond the version came back (hub app, or STATS_TOKEN accepted)
}

// fetchSite reads a remote board's public /health and /version, then
// /api/stats when a token is configured. Failures land in Health.
func fetchSite(ctx context.Context, c *http.Client, s remoteSite, token string) siteRow {
	row := siteRow{Name: s.Name, Board: s.URL + "/"}
	get := func(path, bearer string, v any) error {
		req, err := http.NewRequestWithContext(ctx, "GET", s.URL+path, nil)
		if err != nil {
			return err
		}
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		resp, err := c.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if err := json.NewDecoder(resp.Body).Decode(v); err != nil && resp.StatusCode == http.StatusOK {
			return err
		}
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("HTTP %d", resp.StatusCode)
		}
		return nil
	}
	var health struct{ Status, SHA string }
	if err := get("/health", "", &health); err != nil && health.Status == "" {
		row.Health = err.Error()
	} else {
		row.Health, row.Stats.GitSHA = health.Status, health.SHA
	}
	var v struct{ Version string }
	if get("/version", "", &v) == nil {
		row.Stats.Version = v.Version
	}
	if token != "" {
		var st siteStats
		if err := get("/api/stats", token, &st); err == nil {
			row.Stats, row.Detail = st, true
		}
	}
	return row
}

// sites is the platform admin's view of every board: the hub's apps and the SITES list.
func (h *hub) sites(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	tenants, err := h.reg.Tenants(ctx)
	if err != nil {
		slog.Error("tenants", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	var rows []siteRow
	for _, t := range tenants {
		if t.Status == "deleted" {
			continue
		}
		row := siteRow{Name: t.Name, Board: "/" + t.Slug + "/", App: true, Health: t.Status}
		if st, err := h.reg.Store(ctx, t.Slug); err != nil {
			row.Health = err.Error()
		} else if row.Stats, err = statsFor(ctx, st, h.cfg.Version, h.cfg.GitSHA); err != nil {
			row.Health = err.Error()
		} else {
			row.Detail = true
		}
		rows = append(rows, row)
	}
	remote := make([]siteRow, len(h.cfg.Sites))
	var wg sync.WaitGroup
	client := &http.Client{Timeout: 5 * time.Second}
	for i, s := range h.cfg.Sites {
		wg.Add(1)
		go func() {
			defer wg.Done()
			remote[i] = fetchSite(ctx, client, s, h.cfg.StatsToken)
		}()
	}
	wg.Wait()
	rows = append(rows, remote...)
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].App && !rows[j].App })
	total := 0.0
	for _, r := range rows {
		total += r.Stats.Spend30d
	}
	h.render(w, r, "sites", "Sites", map[string]any{"Sites": rows, "Spend30d": total, "Token": h.cfg.StatsToken != ""})
}
