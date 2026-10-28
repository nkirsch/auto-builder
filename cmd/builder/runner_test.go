package main

import (
	"context"
	"net/http"
	"os"
	"testing"

	"github.com/nkirsch/auto-builder/internal/db"
	"github.com/nkirsch/auto-builder/internal/gate"
)

func TestRunnerToken(t *testing.T) {
	u := os.Getenv("TEST_DATABASE_URL")
	if u == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	pool, err := db.Open(ctx, u)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(ctx, `TRUNCATE runs, run_items RESTART IDENTITY CASCADE`); err != nil {
		t.Fatal(err)
	}
	st := &db.Store{Pool: pool}
	s := &server{cfg: config{Repo: "o/r", GitSHA: "abc"}, store: st, version: func() string { return "" }}
	mux := http.NewServeMux()
	s.runnerRoutes(mux, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))

	run, _ := st.CreateRun(ctx, "now", "nkirsch")
	st.SetRunTokenHash(ctx, run, gate.HashRunToken("good"))

	check := func(token, method, path string, want int) {
		t.Helper()
		if w := runnerDo(mux, token, method, path, "{}"); w.Code != want {
			t.Fatalf("%s %s with %q: %d %s, want %d", method, path, token, w.Code, w.Body, want)
		}
	}
	check("", "POST", "/mcp", http.StatusUnauthorized)
	check("bad", "POST", "/mcp", http.StatusUnauthorized)
	check("good", "POST", "/mcp", http.StatusOK)
	check("good", "POST", "/api/runner/runs/99/claim", http.StatusForbidden)
	check("good", "POST", "/api/runner/runs/1/claim", http.StatusOK)
	check("good", "POST", "/mcp", http.StatusOK) // running
	st.FinishRun(ctx, run, "done", "", "", nil)
	check("good", "POST", "/mcp", http.StatusUnauthorized)
}
