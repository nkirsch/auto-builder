package main

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
)

// hubOverview is one hub's state for the multi-hub view: every board it
// lists (its apps and SITES) with what needs attention. A hub serves its own
// at GET /api/overview behind STATS_TOKEN; /admin/hubs gathers HUBS'.
type hubOverview struct {
	Name     string         `json:"name"`
	URL      string         `json:"url"`
	Version  string         `json:"version,omitempty"`
	Upstream string         `json:"upstream,omitempty"` // the repo the platform repo keeps up with, when its lag is known
	Behind   int            `json:"behind"`             // commits the platform repo lacks of Upstream
	More     bool           `json:"behind_more,omitempty"`
	Sites    []overviewSite `json:"sites"`
	Err      string         `json:"-"` // fetching it failed
	// Unreleased counts the commits on the platform repo's master that the
	// hub's live GIT_SHA lacks; ReleaseKnown says it was counted (older hubs
	// and ones without the App leave both out).
	Unreleased   int  `json:"unreleased,omitempty"`
	ReleaseKnown bool `json:"release_known,omitempty"`
}

// overviewSite is one board on a hub's overview; URLs are absolute.
type overviewSite struct {
	Name       string    `json:"name"`
	Board      string    `json:"board"` // ends in /
	Site       string    `json:"site,omitempty"`
	Logs       string    `json:"logs,omitempty"`
	App        bool      `json:"app"` // the hub's own app, else a SITES board
	Health     string    `json:"health"`
	HealthErr  string    `json:"health_err,omitempty"`
	Detail     bool      `json:"detail"`   // Stats beyond the version came back
	Releases   bool      `json:"releases"` // deploys on release: Unreleased counts
	Unreleased int       `json:"unreleased"`
	Stats      siteStats `json:"stats"`
	// Errors24h counts the app's 5xx responses over the last day, from its
	// request log view; ErrorsKnown says it was counted.
	Errors24h   int  `json:"errors_24h,omitempty"`
	ErrorsKnown bool `json:"errors_known,omitempty"`
}

// SiteHost is the app's site without its scheme, for its link.
func (s overviewSite) SiteHost() string { return siteRow{Site: s.Site}.SiteHost() }

// Healthy reports whether the board's health needs no attention.
func (s overviewSite) Healthy() bool { return s.Health == "ok" || s.Health == "ready" }

// overview is this hub's hubOverview.
func (h *hub) overview(ctx context.Context) (hubOverview, error) {
	rows, err := h.siteRows(ctx)
	if err != nil {
		return hubOverview{}, err
	}
	o := hubOverview{Name: h.host, URL: h.cfg.BaseURL, Version: h.cfg.Version, Sites: make([]overviewSite, len(rows))}
	if h.upstream != nil {
		if lag := h.lag(ctx); lag.Known {
			o.Upstream, o.Behind, o.More = h.cfg.Upstream, lag.N, lag.More
		}
	}
	if rel := h.unreleased(ctx); rel.Known {
		o.Unreleased, o.ReleaseKnown = rel.N, true
	}
	var wg sync.WaitGroup
	for i, r := range rows {
		s := overviewSite{Name: r.Name, Board: r.Board, Site: r.Site, App: r.App, Health: r.Health, HealthErr: r.HealthErr,
			Detail: r.Detail, Stats: r.Stats}
		if r.App {
			s.Name, s.Board = cmp.Or(r.Name, r.Slug), h.cfg.BaseURL+r.Board
			if h.cfg.LogProject != "" {
				s.Logs = s.Board + "logs"
			}
			if s.Healthy() {
				if rt, err := h.tenantFor(ctx, r.Slug); err == nil {
					s.Unreleased, s.Releases = rt.b.PendingRelease(ctx)
				}
			}
		}
		o.Sites[i] = s
		if r.App {
			wg.Add(1)
			go func() {
				defer wg.Done()
				o.Sites[i].Errors24h, o.Sites[i].ErrorsKnown = h.serverErrors(ctx, r.Slug)
			}()
		}
	}
	wg.Wait()
	return o, nil
}

// unreleased counts the commits on the platform repo's master that this
// hub's GIT_SHA lacks, at most once per lagTTL; a failure isn't cached.
func (h *hub) unreleased(ctx context.Context) upstreamLag {
	if h.gh == nil || h.cfg.GitSHA == "" || h.cfg.GitSHA == "dev" {
		return upstreamLag{}
	}
	h.releasedMu.Lock()
	defer h.releasedMu.Unlock()
	if h.releasedLag.Known && time.Since(h.releasedAt) < lagTTL {
		return h.releasedLag
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, n, err := h.gh.Unreleased(ctx, h.cfg.Repo, h.cfg.GitSHA)
	if err != nil {
		slog.Error("platform repo unreleased", "err", err)
		return upstreamLag{}
	}
	h.releasedLag, h.releasedAt = upstreamLag{Known: true, N: n}, time.Now()
	return h.releasedLag
}

// errCount is an app's cached 5xx count; ok is false when Logging couldn't say.
type errCount struct {
	n  int
	ok bool
	at time.Time
}

const (
	errorsWindow = 24 * time.Hour
	errorsTTL    = 10 * time.Minute
)

// serverErrors counts an app's 5xx responses over the last errorsWindow from
// its <slug>-requests view in LOG_PROJECT, at most once per errorsTTL (a
// failure too, so a missing view doesn't slow every page); ok is false
// without a view or a count.
func (h *hub) serverErrors(ctx context.Context, slug string) (n int, ok bool) {
	if h.cfg.LogProject == "" {
		return 0, false
	}
	h.errsMu.Lock()
	c, hit := h.errs[slug]
	h.errsMu.Unlock()
	if hit && time.Since(c.at) < errorsTTL {
		return c.n, c.ok
	}
	count := h.countErrs
	if count == nil {
		count = func(ctx context.Context, view string, since time.Time) (int, error) {
			return logView{name: view, client: &http.Client{Timeout: 10 * time.Second}}.serverErrors(ctx, since)
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	n, err := count(ctx, logViewName(h.cfg.LogProject, slug+"-requests"), time.Now().Add(-errorsWindow))
	if err != nil {
		slog.Warn("server errors", "slug", slug, "err", err)
	}
	c = errCount{n: n, ok: err == nil, at: time.Now()}
	h.errsMu.Lock()
	if h.errs == nil {
		h.errs = map[string]errCount{}
	}
	h.errs[slug] = c
	h.errsMu.Unlock()
	return c.n, c.ok
}

func (h *hub) apiOverview(w http.ResponseWriter, r *http.Request) {
	o, err := h.overview(r.Context())
	if err != nil {
		slog.Error("overview", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, o)
}

// fetchHub reads another hub's /api/overview; a failure lands in Err.
func fetchHub(ctx context.Context, c *http.Client, hub remoteSite, token string) hubOverview {
	o := hubOverview{Name: hub.Name, URL: hub.URL}
	req, err := http.NewRequestWithContext(ctx, "GET", hub.URL+"/api/overview", nil)
	if err != nil {
		o.Err = err.Error()
		return o
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := c.Do(req)
	if err != nil {
		o.Err = shortErr(err)
		return o
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		o.Err = fmt.Sprintf("HTTP %d", resp.StatusCode)
		if resp.StatusCode == http.StatusUnauthorized {
			o.Err += ": does it have this hub's STATS_TOKEN?"
		}
		return o
	}
	if err := json.NewDecoder(resp.Body).Decode(&o); err != nil {
		o.Err = "bad overview: " + err.Error()
	}
	o.Name, o.URL = hub.Name, hub.URL // HUBS names it
	return o
}

// attentionRow is an item needing action on the multi-hub view.
type attentionRow struct {
	attention
	Hub, Site, URL string
}

// kindRank orders attention: urgent, then escalated, blocked, and boards
// whose runs keep failing.
var kindRank = map[string]int{"urgent": 0, "escalated": 1, "blocked": 2, "failing": 3}

// failingAfter is how many runs in a row must fail to list a board as failing.
const failingAfter = 2

// hubs is the multi-hub view: what needs attention on every hub's boards
// first, then each hub's boards with their health and release state.
func (h *hub) hubs(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	var remote []remoteSite
	for _, hub := range h.cfg.Hubs {
		if strings.TrimSuffix(hub.URL, "/") != strings.TrimSuffix(h.cfg.BaseURL, "/") {
			remote = append(remote, hub)
		}
	}
	all := make([]hubOverview, 1+len(remote))
	var wg sync.WaitGroup
	client := &http.Client{Timeout: 8 * time.Second}
	for i, hub := range remote {
		wg.Add(1)
		go func() {
			defer wg.Done()
			all[1+i] = fetchHub(ctx, client, hub, h.cfg.StatsToken)
		}()
	}
	var err error
	if all[0], err = h.overview(ctx); err != nil {
		slog.Error("overview", "err", err)
		all[0] = hubOverview{Name: h.host, URL: h.cfg.BaseURL, Err: err.Error()}
	}
	wg.Wait()
	var rows []attentionRow
	other := 0
	for _, o := range all {
		for _, s := range o.Sites {
			other += s.Stats.Other
			for _, a := range s.Stats.Attention {
				rows = append(rows, attentionRow{attention: a, Hub: o.Name, Site: s.Name, URL: fmt.Sprintf("%sitems/%d", s.Board, a.ID)})
			}
			if n := s.Stats.FailStreak; n >= failingAfter {
				a := attention{Kind: "failing", Title: fmt.Sprintf("Last %d runs failed", n)}
				if s.Stats.LastRun != nil {
					a.At = s.Stats.LastRun.At
				}
				rows = append(rows, attentionRow{attention: a, Hub: o.Name, Site: s.Name, URL: s.Board + "runs"})
			}
		}
	}
	slices.SortStableFunc(rows, func(a, b attentionRow) int {
		return cmp.Or(cmp.Compare(kindRank[a.Kind], kindRank[b.Kind]), a.At.Compare(b.At))
	})
	h.render(w, r, "hubs", "Hubs", map[string]any{"Hubs": all, "Attention": rows, "Other": other,
		"Token": h.cfg.StatsToken != "", "Configured": len(remote) > 0})
}
