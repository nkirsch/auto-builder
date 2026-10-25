package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
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
