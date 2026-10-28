package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const chrome = "Mozilla/5.0 (Macintosh) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/150.0 Safari/537.36"

func TestBuildReport(t *testing.T) {
	at := time.Date(2026, 10, 24, 12, 0, 0, 0, pacific)
	get := func(ip, ua, path, ref string) hit {
		return hit{At: at, IP: ip, UA: ua, Path: path, Ref: ref, Method: "GET", Status: 200}
	}
	hits := []hit{
		get("1.1.1.1", chrome, "/", "https://www.google.com/"),
		get("1.1.1.1", chrome, "/css/style.css", "https://site.example/"),
		get("1.1.1.1", chrome, "/posts/a.html", "https://site.example/"),
		get("2.2.2.2", chrome, "/", ""),       // never fetched an asset: scraper
		get("3.3.3.3", "GPTBot/1.0", "/", ""), // declared bot
		get("4.4.4.4", chrome, "/", ""),       // home
		get("4.4.4.4", chrome, "/css/style.css", ""),
		get("1.1.1.1", chrome, "/iterate/", ""), // board, not site
	}
	r := buildReport(hits, "site.example", at.AddDate(0, 0, -2), at, map[string]bool{"4.4.4.4": true})
	if r.Visitors != 1 || r.Views != 2 || r.Dropped != 3 {
		t.Errorf("visitors=%d views=%d dropped=%d, want 1 2 3", r.Visitors, r.Views, r.Dropped)
	}
	if len(r.Days) != 3 || r.Days[0].Key != "2026-10-24" || r.Days[0].Views != 2 || r.Days[1].Views != 0 {
		t.Errorf("days = %+v", r.Days)
	}
	if len(r.Referrers) != 1 || r.Referrers[0].Key != "www.google.com" {
		t.Errorf("referrers = %+v", r.Referrers)
	}
}

func TestTrafficCaches(t *testing.T) {
	calls := 0
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, pacific)
	tr := &traffic{host: "site.example", now: func() time.Time { return now },
		home: func(context.Context) map[string]bool { return nil },
		hits: func(context.Context, time.Time) ([]hit, error) { calls++; return nil, nil }}
	for _, path := range []string{"/logs", "/logs", "/logs?refresh"} {
		w := httptest.NewRecorder()
		tr.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "page views") || !strings.Contains(w.Body.String(), "<h1>site.example · Traffic</h1>") {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body)
		}
	}
	if calls != 2 {
		t.Errorf("log reads = %d, want 2 (cached, then refresh)", calls)
	}
}

func TestHubTraffic(t *testing.T) {
	h := &hub{cfg: config{LogProject: "kirsch-org"}, host: "kirsch.org"}
	tr := h.traffic("momo")
	if tr.host != "momo.kirsch.org" || tr.view != "projects/kirsch-org/locations/global/buckets/_Default/views/momo-requests" {
		t.Errorf("host=%q view=%q", tr.host, tr.view)
	}
	if tr := (&hub{host: "kirsch.org"}).traffic("momo"); tr.view != "" {
		t.Errorf("no LOG_PROJECT: view=%q", tr.view)
	}
}
