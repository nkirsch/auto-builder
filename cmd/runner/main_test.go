package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	autobuilder "github.com/nkirsch/auto-builder"
)

func TestSummaryPrefersResult(t *testing.T) {
	log := "$ claude\n" + `{"type":"assistant","message":{}}` + "\n" +
		`{"type":"result","result":"Opened PR #4 for item 2.","total_cost_usd":1.2345}` + "\n"
	got, cost := result([]byte(log))
	if got != "Opened PR #4 for item 2." {
		t.Fatalf("got %q", got)
	}
	if cost == nil || *cost != 1.2345 {
		t.Fatalf("cost = %v", cost)
	}
}

func TestSummaryTail(t *testing.T) {
	log := strings.Repeat("x", 5000) + "END"
	got, cost := result([]byte(log))
	if cost != nil {
		t.Fatalf("cost = %v, want nil", *cost)
	}
	if len(got) != summaryLen || !strings.HasSuffix(got, "END") {
		t.Fatalf("len %d, suffix %q", len(got), got[len(got)-3:])
	}
}

func TestCapped(t *testing.T) {
	c := &capped{max: 5}
	c.Write([]byte("abc"))
	c.Write([]byte("defg"))
	if c.String() != "abcde" {
		t.Fatalf("got %q", c.String())
	}
}

func TestGitHubToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/runner/runs/7/token" || r.Header.Get("Authorization") != "Bearer rt" {
			http.Error(w, "no", http.StatusConflict)
			return
		}
		w.Write([]byte(`{"token":"ghs_x","expires_at":"2026-10-25T03:00:00Z","name":"app[bot]","email":"1+app[bot]@users.noreply.github.com"}`))
	}))
	defer srv.Close()
	a := &api{base: srv.URL, token: "rt", client: srv.Client()}
	got, err := a.githubToken(context.Background(), "7")
	if err != nil {
		t.Fatal(err)
	}
	if got.Token != "ghs_x" || got.Name != "app[bot]" || got.Email != "1+app[bot]@users.noreply.github.com" {
		t.Fatalf("got %+v", got)
	}
	if _, err := a.githubToken(context.Background(), "8"); err == nil || !strings.Contains(err.Error(), "409") {
		t.Fatalf("refused token: err = %v", err)
	}
}

func TestChildEnvDropsAppSecrets(t *testing.T) {
	got := childEnv([]string{"PATH=/bin", "GH_APP_PRIVATE_KEY=k", "GH_APP_ID=1", "RUNNER_TOKEN=rt"})
	if !slices.Equal(got, []string{"PATH=/bin", "RUNNER_TOKEN=rt"}) {
		t.Fatalf("got %v", got)
	}
}

func TestInstallCommand(t *testing.T) {
	repo, home := t.TempDir(), t.TempDir()
	if ok, err := installCommand(repo, home); err != nil || !ok {
		t.Fatalf("tenant repo: %v %v", ok, err)
	}
	b, err := os.ReadFile(filepath.Join(home, ".claude", "commands", "build-board.md"))
	if err != nil || len(b) == 0 || string(b) != autobuilder.BuildBoard {
		t.Fatalf("installed %d bytes, %v", len(b), err)
	}
	own := filepath.Join(repo, ".claude", "commands")
	os.MkdirAll(own, 0o755)
	os.WriteFile(filepath.Join(own, "build-board.md"), []byte("site's own"), 0o644)
	if ok, err := installCommand(repo, t.TempDir()); err != nil || ok {
		t.Fatalf("repo with its own command: %v %v", ok, err)
	}
}
