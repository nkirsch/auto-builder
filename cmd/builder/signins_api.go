package main

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nkirsch/auto-builder/internal/auth"
	"github.com/nkirsch/auto-builder/internal/db"
)

// serveSignIns lists successful sign-ins over the last ?days=N (default 7, at
// most db.SignInKeep), newest first, then the IPs + browsers sessions used, so
// a visitor report can name who they came from.
func serveSignIns(all func(context.Context, time.Time) ([]db.SignIn, error), used func(context.Context, time.Time) ([]db.Seen, error)) http.HandlerFunc {
	type signIn struct {
		At        time.Time `json:"at"`
		Identity  string    `json:"identity"`
		Board     string    `json:"board,omitempty"`
		IP        string    `json:"ip"`
		UserAgent string    `json:"ua"`
	}
	return func(w http.ResponseWriter, r *http.Request) {
		days := 7
		if d, err := strconv.Atoi(r.URL.Query().Get("days")); err == nil && d >= 1 && d <= int(db.SignInKeep.Hours()/24) {
			days = d
		}
		since := time.Now().Add(-time.Duration(days) * 24 * time.Hour)
		rows, err := all(r.Context(), since)
		if err != nil {
			slog.Error("signins", "err", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		out := []signIn{}
		for _, s := range rows {
			if !s.Failed() && s.Identity != "" {
				out = append(out, signIn{s.At, s.Identity, s.Board, s.IP, s.UserAgent})
			}
		}
		// Sessions outlive the IP they signed in from.
		seen, err := used(r.Context(), since)
		if err != nil {
			slog.Error("seen", "err", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		for _, s := range seen {
			out = append(out, signIn{s.Last, s.Identity, "", s.IP, s.UserAgent})
		}
		writeJSON(w, http.StatusOK, out)
	}
}

// seenEvery spaces out seenNoter's writes for one identity, IP and browser.
const seenEvery = time.Hour

// seenNoter records the IP + browser of each signed-in request (pages, not
// assets or APIs), at most once per seenEvery each.
type seenNoter struct {
	mu sync.Mutex
	at map[string]time.Time // identity + IP + UA → when last recorded
}

func (n *seenNoter) wrap(next http.Handler, who func(*http.Request) string, record func(ctx context.Context, identity, ip, ua string) error) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r)
		p := r.URL.Path
		if strings.HasPrefix(p, "/static/") || strings.HasPrefix(p, "/api/") || p == "/health" || p == "/favicon.ico" {
			return
		}
		id := who(r)
		if id == "" {
			return
		}
		ip, ua := auth.ClientIP(r), r.UserAgent()
		key := id + "\x00" + ip + "\x00" + ua
		now := time.Now()
		n.mu.Lock()
		if n.at == nil {
			n.at = map[string]time.Time{}
		}
		if now.Sub(n.at[key]) < seenEvery {
			n.mu.Unlock()
			return
		}
		n.at[key] = now
		n.mu.Unlock()
		if err := record(context.WithoutCancel(r.Context()), id, ip, ua); err != nil {
			slog.Error("record seen", "err", err)
		}
	})
}

// signIns serves this board's sign-ins and sessions' IPs + browsers.
func (s *server) signIns() http.Handler {
	return serveSignIns(func(ctx context.Context, since time.Time) ([]db.SignIn, error) {
		return db.AllSignIns(ctx, s.store.Pool, "", "", since)
	}, func(ctx context.Context, since time.Time) ([]db.Seen, error) {
		return db.AllSeen(ctx, s.store.Pool, since)
	})
}

func (s *server) recordSeen(ctx context.Context, identity, ip, ua string) error {
	return db.RecordSeen(ctx, s.store.Pool, identity, ip, ua)
}
