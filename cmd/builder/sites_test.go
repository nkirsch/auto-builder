package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestParseSites(t *testing.T) {
	got, err := parseSites(" builder=https://builder.extreme.org/  nick.org=https://www.nick.org/iterate,")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != (remoteSite{"builder", "https://builder.extreme.org"}) || got[1].URL != "https://www.nick.org/iterate" {
		t.Fatalf("got %+v", got)
	}
	if _, err := parseSites("nope"); err == nil {
		t.Error("want an error for an entry without a URL")
	}
	if got, _ := parseSites(""); len(got) != 0 {
		t.Errorf("empty SITES = %+v", got)
	}
}

func TestFetchSite(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/iterate/health":
			json.NewEncoder(w).Encode(map[string]string{"status": "ok", "sha": "abc1234def"})
		case "/iterate/version":
			json.NewEncoder(w).Encode(map[string]string{"version": "v26.10.25-5"})
		case "/iterate/api/stats":
			if r.Header.Get("Authorization") != "Bearer tok" {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			json.NewEncoder(w).Encode(siteStats{Version: "v26.10.25-5", Open: 3, Spend30d: 1.5, LastRun: &runBrief{Status: "done"}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	s := remoteSite{Name: "nick.org", URL: srv.URL + "/iterate"}

	row := fetchSite(context.Background(), srv.Client(), s, "tok")
	if row.Health != "ok" || !row.Detail || row.Stats.Open != 3 || row.Stats.Spend30d != 1.5 || row.Stats.LastRun.Status != "done" || row.Board != srv.URL+"/iterate/" {
		t.Fatalf("with token: %+v", row)
	}
	row = fetchSite(context.Background(), srv.Client(), s, "wrong")
	if row.Detail || row.Stats.Version != "v26.10.25-5" || row.Stats.GitSHA != "abc1234def" || row.Health != "ok" {
		t.Fatalf("bad token: %+v", row)
	}
	row = fetchSite(context.Background(), srv.Client(), remoteSite{Name: "gone", URL: srv.URL + "/missing"}, "")
	if row.Health == "ok" || !strings.Contains(row.Health, "404") {
		t.Fatalf("missing site health = %q", row.Health)
	}
}

func TestHubSites(t *testing.T) {
	ctx := context.Background()
	h := testHub(t)
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			json.NewEncoder(w).Encode(map[string]string{"status": "ok", "sha": "fff0000aaa"})
		case "/version":
			json.NewEncoder(w).Encode(map[string]string{"version": "v26.10.20-1"})
		case "/api/stats":
			if r.Header.Get("Authorization") != "Bearer tok" {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			json.NewEncoder(w).Encode(siteStats{Version: "v26.10.20-1", Open: 4, Spend30d: 2.25})
		}
	}))
	defer remote.Close()
	h.cfg.Sites, h.cfg.StatsToken = []remoteSite{{Name: "momo", URL: remote.URL}}, "tok"
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/alpha/health" {
			json.NewEncoder(w).Encode(map[string]string{"status": "ok", "sha": "aaa1111bbb"})
		}
	}))
	defer app.Close()
	h.appBase = func(slug string) string { return app.URL + "/" + slug }
	srv := h.routes()
	if w := do(h, srv, "nkirsch", "POST", "/new", url.Values{"slug": {"alpha"}, "name": {"Alpha"}}); w.Code != 303 {
		t.Fatalf("create: %d", w.Code)
	}
	h.reg.SetStatus(ctx, "alpha", "ready", "")
	if w := do(h, srv, "nick@example.org", "POST", "/alpha/items", url.Values{"title": {"Dark mode"}, "body": {"please"}}); w.Code != 303 {
		t.Fatalf("item: %d", w.Code)
	}

	// Sites merged into Apps: the old path redirects, and members see only their list.
	if w := do(h, srv, "mallory", "GET", "/admin/sites", nil); w.Code != 301 || w.Header().Get("Location") != "/" {
		t.Fatalf("old sites path: %d %q", w.Code, w.Header().Get("Location"))
	}
	if body := do(h, srv, "mallory", "GET", "/", nil).Body.String(); strings.Contains(body, "momo") || strings.Contains(body, "30-day") {
		t.Errorf("a non-admin sees every board: %s", body)
	}
	w := do(h, srv, "nkirsch", "GET", "/", nil)
	body := w.Body.String()
	for _, want := range []string{
		`<a href="/alpha/"><b>alpha</b></a> <span class="muted">Alpha</span> · <a href="` + app.URL + `/alpha">site</a> <span class="badge">admin</span>`,
		"aaa1111", `<td class="num">1</td>`,
		`<a href="` + remote.URL + `/"><b>momo</b></a> <span class="muted">standalone</span>`, "v26.10.20-1", `<td class="num">4</td>`, "$2.25", "<b>$2.25</b>",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("apps page missing %q", want)
		}
	}
	if w.Code != 200 || strings.Contains(body, `href="/admin/sites"`) {
		t.Fatalf("apps: %d, or still links Sites", w.Code)
	}
}
