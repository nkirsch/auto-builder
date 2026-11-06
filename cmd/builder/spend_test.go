package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/nkirsch/auto-builder/internal/db"
)

func TestParseSpendReporters(t *testing.T) {
	got, err := parseSpendReporters(" a=one  b=two\n")
	if err != nil || len(got) != 2 || got[1] != (spendReporter{"b", "two"}) {
		t.Fatalf("got %+v, %v", got, err)
	}
	for _, bad := range []string{"a", "=x", "a=", "a=1 a=2", "a=1 b=1"} {
		if _, err := parseSpendReporters(bad); err == nil {
			t.Errorf("%q: want an error", bad)
		}
	}
}

// postSpend sends a report to the hub with token.
func postSpend(h *hub, srv http.Handler, token, body string) int {
	r := httptest.NewRequest("POST", "/api/spend", strings.NewReader(body))
	r.Host = "hub.example"
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, r)
	return w.Code
}

func TestSpendReports(t *testing.T) {
	ctx := context.Background()
	h := testHub(t)
	h.cfg.SpendReporters = []spendReporter{{"child", "child-token"}}
	srv := h.routes()

	for _, c := range []struct {
		token, body string
		want        int
	}{
		{"", `{"total_usd":1}`, 401},
		{"wrong", `{"total_usd":1}`, 401},
		{"child-token", `{}`, 400},
		{"child-token", `{"total_usd":-1}`, 400},
		{"child-token", `{"total_usd":1e9}`, 400},
		{"child-token", `nope`, 400},
	} {
		if got := postSpend(h, srv, c.token, c.body); got != c.want {
			t.Errorf("%q %s = %d, want %d", c.token, c.body, got, c.want)
		}
	}
	// Each report replaces the last.
	for _, body := range []string{`{"total_usd":5}`, `{"total_usd":7.5}`} {
		if got := postSpend(h, srv, "child-token", body); got != 204 {
			t.Fatalf("report = %d", got)
		}
	}
	if total, ok, err := h.reg.SpendTotal(ctx); err != nil || !ok || total != 7.5 {
		t.Fatalf("total = %v %v %v", total, ok, err)
	}
}

func TestSpendPublicPages(t *testing.T) {
	ctx := context.Background()
	h := testHub(t)
	srv := h.routes()
	const shown = "Agents have spent"
	if err := h.reg.SetSpendReport(ctx, "child", 2.5); err != nil {
		t.Fatal(err)
	}
	if body := do(h, srv, "", "GET", "/auth/login", nil).Body.String(); strings.Contains(body, shown) {
		t.Error("total shown without SPEND_PUBLIC")
	}
	h.cfg.SpendPublic = true
	tick := httptest.NewRequest("POST", "/api/gate/tick", nil)
	tick.Host = "hub.example"
	tick.Header.Set("Authorization", "Bearer tick")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, tick)
	if w.Code != 200 || strings.Contains(w.Body.String(), "partial") {
		t.Fatalf("tick: %d %s", w.Code, w.Body)
	}
	for _, c := range []struct{ who, path string }{{"", "/auth/login"}, {"nkirsch", "/"}, {"mallory", "/"}} {
		if body := do(h, srv, c.who, "GET", c.path, nil).Body.String(); !strings.Contains(body, "<b>$2.50</b>") {
			t.Errorf("%q %s: no total in %.300q", c.who, c.path, body)
		}
	}
}

// A hub's report carries its own spend, deleted apps included, plus its children's.
func TestSyncSpendReportsUp(t *testing.T) {
	ctx := context.Background()
	h := testHub(t)
	srv := h.routes()
	if w := do(h, srv, "nkirsch", "POST", "/new", map[string][]string{"slug": {"alpha"}, "name": {"Alpha"}, "repo": {"o/alpha"}, "existing": {"on"}}); w.Code != 303 {
		t.Fatalf("create = %d", w.Code)
	}
	st, err := h.reg.Store(ctx, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Pool.Exec(ctx, `INSERT INTO runs (trigger, status, cost_usd) VALUES ('manual', 'done', 1.25)`); err != nil {
		t.Fatal(err)
	}
	if err := h.reg.SetSpendReport(ctx, "child", 2); err != nil {
		t.Fatal(err)
	}
	if err := h.reg.SetStatus(ctx, "alpha", "deleted", ""); err != nil {
		t.Fatal(err)
	}

	var got map[string]float64
	var auth string
	parent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer parent.Close()
	h.cfg.SpendReportTo, h.cfg.SpendReportToken = parent.URL, "up-token"
	if err := h.syncSpend(ctx); err != nil {
		t.Fatal(err)
	}
	if got["total_usd"] != 3.25 || auth != "Bearer up-token" {
		t.Fatalf("sent %v with %q", got, auth)
	}

	h.cfg.SpendReportTo = "http://127.0.0.1:1"
	if err := h.syncSpend(ctx); err == nil {
		t.Error("want an error when the parent is unreachable")
	}
}

// A standalone board reports its own lifetime spend, and nothing when unset.
func TestBoardSyncSpend(t *testing.T) {
	ctx := context.Background()
	u := os.Getenv("TEST_DATABASE_URL")
	if u == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	pool, err := db.Open(ctx, u)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(ctx, `TRUNCATE runs, run_items RESTART IDENTITY CASCADE`); err != nil {
		t.Fatal(err)
	}
	st := &db.Store{Pool: pool}
	if _, err := st.Pool.Exec(ctx, `INSERT INTO runs (trigger, status, cost_usd) VALUES ('manual', 'done', 4.5)`); err != nil {
		t.Fatal(err)
	}
	var got map[string]float64
	parent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer parent.Close()
	s := &server{store: st}
	if err := s.syncSpend(ctx); err != nil || got != nil {
		t.Fatalf("unset: sent %v, err %v", got, err)
	}
	s.cfg.SpendReportTo, s.cfg.SpendReportToken = parent.URL, "up-token"
	if err := s.syncSpend(ctx); err != nil || got["total_usd"] != 4.5 {
		t.Fatalf("sent %v, err %v", got, err)
	}
}
