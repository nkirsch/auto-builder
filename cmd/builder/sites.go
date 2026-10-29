package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nkirsch/auto-builder/internal/db"
)

// started approximates the deploy time: Cloud Run starts a fresh instance per revision.
var started = time.Now().UTC()

// siteStats is one board's summary for the hub's Apps page; a single
// board serves it at GET /api/stats behind STATS_TOKEN.
type siteStats struct {
	Version  string    `json:"version"`
	GitSHA   string    `json:"sha"`
	Started  time.Time `json:"started"`
	Open     int       `json:"open"`
	LastRun  *runBrief `json:"last_run,omitempty"`
	Spend30d float64   `json:"spend_30d_usd"`
	// Attention is the unfinished items someone must act on: escalated to the
	// platform admins ("urgent") or the board's admins ("escalated"), or
	// blocked. Other counts the rest of the unfinished items.
	Attention []attention `json:"attention,omitempty"`
	Other     int         `json:"other"`
	// Failed7d counts the runs that failed in the last week; FailStreak the
	// newest finished runs that failed in a row.
	Failed7d   int `json:"failed_7d,omitempty"`
	FailStreak int `json:"fail_streak,omitempty"`
}

// failedWindow and streakRuns bound what statsFor reads of a board's runs.
const (
	failedWindow = 7 * 24 * time.Hour
	streakRuns   = 10
)

// attention is one item needing action, for the multi-hub view.
type attention struct {
	ID     int       `json:"id"`
	Title  string    `json:"title"`
	Author string    `json:"author"`
	Kind   string    `json:"kind"` // urgent, escalated or blocked
	Note   string    `json:"note,omitempty"`
	At     time.Time `json:"at"` // escalated or blocked, else last updated
}

// attentionOf is it as an attention item, or false when nobody must act.
func attentionOf(it db.Item) (attention, bool) {
	a := attention{ID: it.ID, Title: it.Title, Author: it.Author, At: it.UpdatedAt}
	switch {
	case it.Escalation == "platform":
		a.Kind = "urgent"
	case it.Escalation != "":
		a.Kind = "escalated"
	case it.Blocked != "":
		a.Kind, a.Note = "blocked", it.Blocked
		if it.BlockedAt != nil {
			a.At = *it.BlockedAt
		}
		return a, true
	default:
		return a, false
	}
	if it.EscalatedAt != nil {
		a.At = *it.EscalatedAt
	}
	return a, true
}

type runBrief struct {
	Status string    `json:"status"`
	At     time.Time `json:"at"` // finished, else created
}

func statsFor(ctx context.Context, st *db.Store, version, sha string) (siteStats, error) {
	s := siteStats{Version: version, GitSHA: sha, Started: started}
	items, err := st.Items(ctx)
	if err != nil {
		return s, err
	}
	for _, it := range items {
		if it.Status == "open" {
			s.Open++
		}
		if (it.Status == "shipped" || it.Status == "closed") && it.Blocked == "" {
			continue // done
		}
		if a, ok := attentionOf(it); ok {
			s.Attention = append(s.Attention, a)
		} else {
			s.Other++
		}
	}
	runs, err := st.Runs(ctx, streakRuns)
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
	s.FailStreak = failStreak(runs)
	rs, err := st.RunStats(ctx, time.Now().Add(-failedWindow))
	if err != nil {
		return s, err
	}
	s.Failed7d = rs.Failed
	c, err := st.RunCosts(ctx)
	s.Spend30d = c.Last30Days
	return s, err
}

// failStreak counts the newest runs (newest first) that failed in a row,
// passing over runs still in flight or cancelled.
func failStreak(runs []db.Run) int {
	n := 0
	for _, r := range runs {
		if r.Status == "done" {
			break
		}
		if r.Status == "failed" {
			n++
		}
	}
	return n
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

// member answers the hub's "what's identity's role here?" (GET
// /api/member?identity=, behind STATS_TOKEN): "admin", "member" or "".
func (s *server) member(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"role": roleOf(r.Context(), s.store, r.URL.Query().Get("identity"))})
}

// roleOf is identity's (a GitHub login or a Google address) role on a board, or "".
func roleOf(ctx context.Context, st *db.Store, identity string) string {
	identity = strings.TrimSpace(identity)
	if identity == "" {
		return ""
	}
	var m db.Member
	var err error
	if strings.Contains(identity, "@") {
		m, err = st.MemberByEmail(ctx, identity)
	} else {
		m, err = st.Member(ctx, identity)
	}
	if err != nil {
		return ""
	}
	return m.Role
}

// remoteRole asks a single board for identity's role; errors count as none.
func remoteRole(ctx context.Context, c *http.Client, s remoteSite, token, identity string) string {
	req, err := http.NewRequestWithContext(ctx, "GET", s.URL+"/api/member?identity="+url.QueryEscape(identity), nil)
	if err != nil {
		return ""
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := c.Do(req)
	if err != nil {
		slog.Warn("remote role", "site", s.Name, "err", err)
		return ""
	}
	defer resp.Body.Close()
	var v struct{ Role string }
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&v) != nil {
		slog.Warn("remote role", "site", s.Name, "status", resp.Status)
		return ""
	}
	return v.Role
}

// remoteSite is a single-board site from SITES ("name=board-URL ...", space-
// or comma-separated), or a hub from HUBS (same form).
type remoteSite struct{ Name, URL string }

func parseSites(v string) ([]remoteSite, error) {
	var out []remoteSite
	for _, kv := range strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' }) {
		name, u, ok := strings.Cut(kv, "=")
		if !ok || name == "" || !strings.HasPrefix(u, "https://") && !strings.HasPrefix(u, "http://") {
			return nil, fmt.Errorf("entry %q: want name=https://board", kv)
		}
		out = append(out, remoteSite{Name: strings.TrimSpace(name), URL: strings.TrimSuffix(u, "/")})
	}
	return out, nil
}

// siteRow is one line of the hub's Apps page for platform admins.
type siteRow struct {
	Name, Board string
	Slug, Site  string // hub apps: the slug and the app's own URL
	App         bool   // a hub app, read from its store; else a remote single board
	Health      string // "ok", or why not in a word or two
	HealthErr   string // the full error behind Health, for its tooltip
	Stats       siteStats
	Detail      bool   // Stats beyond the version came back (hub app, or STATS_TOKEN accepted)
	Role        string // the viewer's role on a hub app, "" if none
}

// SiteHost is the app's site without its scheme, for its link.
func (r siteRow) SiteHost() string {
	return strings.TrimPrefix(strings.TrimPrefix(r.Site, "https://"), "http://")
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
		row.Health, row.HealthErr = shortErr(err), err.Error()
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

// shortErr names a failed fetch in a word or two; the page has no room for
// Go's full "Get ...: tls: ..." text.
func shortErr(err error) string {
	var cert *tls.CertificateVerificationError
	var rec tls.RecordHeaderError
	var dns *net.DNSError
	var ne net.Error
	m := err.Error()
	switch {
	case errors.As(err, &cert), errors.As(err, &rec), strings.Contains(m, "tls:"), strings.Contains(m, "x509:"):
		return "TLS error"
	case errors.As(err, &dns):
		return "no DNS"
	case errors.As(err, &ne) && ne.Timeout():
		return "timeout"
	case strings.HasPrefix(m, "HTTP "):
		return m
	case strings.Contains(m, "connection refused"), strings.Contains(m, "connection reset"), strings.Contains(m, "EOF"):
		return "unreachable"
	}
	return "down"
}

// appURL is where an app is served.
func (h *hub) appURL(slug string) string {
	if h.appBase != nil {
		return h.appBase(slug)
	}
	return "https://" + slug + "." + h.host
}

// siteRows is the platform admin's view of every board: the hub's apps and
// the SITES list, with each one's health, version, last run and spend.
func (h *hub) siteRows(ctx context.Context) ([]siteRow, error) {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	tenants, err := h.reg.Tenants(ctx)
	if err != nil {
		return nil, err
	}
	var rows []siteRow
	var wg sync.WaitGroup
	client := &http.Client{Timeout: 5 * time.Second}
	for _, t := range tenants {
		if t.Status == "deleted" {
			continue
		}
		row := siteRow{Name: t.Name, Slug: t.Slug, Site: h.appURL(t.Slug), Board: "/" + t.Slug + "/", App: true, Health: t.Status}
		if st, err := h.reg.Store(ctx, t.Slug); err != nil {
			row.Health = err.Error()
		} else if row.Stats, err = statsFor(ctx, st, "", ""); err != nil {
			row.Health = err.Error()
		} else {
			row.Detail = true
		}
		rows = append(rows, row)
	}
	// A ready app's health and version are its own service's, like a board's.
	for i := range rows {
		if rows[i].Health != "ready" {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			live := fetchSite(ctx, client, remoteSite{URL: rows[i].Site}, "")
			rows[i].Health, rows[i].HealthErr, rows[i].Stats.GitSHA = live.Health, live.HealthErr, live.Stats.GitSHA
		}()
	}
	wg.Wait()
	remote := make([]siteRow, len(h.cfg.Sites))
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
	return rows, nil
}
