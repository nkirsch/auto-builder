package main

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/nkirsch/auto-builder/internal/db"
	"github.com/nkirsch/auto-builder/internal/gate"
)

// runnerRoutes mounts the runner API and /mcp behind the run's own RUN_TOKEN.
func (s *server) runnerRoutes(mux *http.ServeMux, mcp http.Handler) {
	mux.Handle("/mcp", s.runner(false, mcp))
	mux.Handle("POST /api/runner/runs/{id}/claim", s.runner(true, http.HandlerFunc(s.claim)))
	mux.Handle("POST /api/runner/runs/{id}/token", s.runner(true, http.HandlerFunc(s.token)))
	mux.Handle("POST /api/runner/runs/{id}/stage", s.runner(true, http.HandlerFunc(s.stage)))
	mux.Handle("POST /api/runner/runs/{id}/finish", s.runner(true, http.HandlerFunc(s.finish)))
	mux.Handle("GET /api/runner/attachments/{id}", s.runner(false, http.HandlerFunc(s.attachment)))
}

// runner admits a bearer RUN_TOKEN while its run is requested or running;
// with ownRun, only for that run's endpoints.
func (s *server) runner(ownRun bool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || token == "" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		id, err := s.store.RunByTokenHash(r.Context(), gate.HashRunToken(token))
		if errors.Is(err, db.ErrNotFound) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		} else if err != nil {
			slog.Error("run token", "err", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if ownRun && r.PathValue("id") != strconv.Itoa(id) {
			http.Error(w, "token is for another run", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}
