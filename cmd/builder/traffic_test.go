package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nkirsch/auto-builder/internal/tenant"
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
	h := &hub{cfg: config{LogProject: "family-proj"}, host: "family.example"}
	tr := h.traffic("maple")
	if tr.host != "maple.family.example" || tr.view != "projects/family-proj/locations/global/buckets/_Default/views/maple-requests" {
		t.Errorf("host=%q view=%q", tr.host, tr.view)
	}
	if tr := (&hub{host: "family.example"}).traffic("maple"); tr.view != "" {
		t.Errorf("no LOG_PROJECT: view=%q", tr.view)
	}
}

func TestHubTrafficPage(t *testing.T) {
	ctx := context.Background()
	h := testHub(t)
	h.cfg.LogProject = "proj"
	for _, slug := range []string{"alpha", "beta"} {
		if err := h.reg.Create(ctx, tenant.Tenant{Slug: slug, Repo: "o/" + slug, CreatedBy: "nkirsch", Existing: true}); err != nil {
			t.Fatal(err)
		}
	}
	at := time.Now()
	hits := []hit{
		{At: at, IP: "1.1.1.1", UA: chrome, Path: "/", Method: "GET", Status: 200},
		{At: at, IP: "1.1.1.1", UA: chrome, Path: "/css/a.css", Method: "GET", Status: 200},
	}
	alpha := h.traffic("alpha")
	alpha.home = func(context.Context) map[string]bool { return nil }
	alpha.hits = func(context.Context, time.Time) ([]hit, error) { return hits, nil }
	h.traffic("beta").hits = func(context.Context, time.Time) ([]hit, error) { return nil, fmt.Errorf("view missing") }

	srv := h.routes()
	if w := do(h, srv, "mallory", "GET", "/admin/traffic", nil); w.Code != http.StatusForbidden {
		t.Fatalf("non-admin: %d", w.Code)
	}
	w := do(h, srv, "nkirsch", "GET", "/admin/traffic", nil)
	body := w.Body.String()
	for _, want := range []string{`href="/alpha/logs"`, "alpha.hub.example", `<td class="num">1</td>`, `title="view missing"`, `href="/admin/traffic"`} {
		if w.Code != http.StatusOK || !strings.Contains(body, want) {
			t.Fatalf("missing %q (%d):\n%s", want, w.Code, body)
		}
	}
	// The board's /logs shares the report built for the hub page.
	if h.traffic("alpha") != alpha {
		t.Error("traffic not cached per app")
	}
}

func TestLookupHomeUnset(t *testing.T) {
	if m := lookupHome("")(context.Background()); len(m) != 0 {
		t.Errorf("no HOME_HOST: %v", m)
	}
	if m := lookupHome("localhost")(context.Background()); !m["127.0.0.1"] && !m["::1"] {
		t.Errorf("localhost: %v", m)
	}
}

func TestPage(t *testing.T) {
	for path, want := range map[string]bool{
		"/": true, "/posts/a.html": true, "/posts/": true, "/book": true, "/i/abc123": true,
		"/css/a.css": false, "/a.png": false, "/iterate/items": false, "/api/slots": false, "/auth/google": false,
		"/static/x": false, "/health": false, "/version": false, "/.well-known/x": false, "/mcp": false,
	} {
		if got := page(hit{Method: "GET", Status: 200, Path: path}); got != want {
			t.Errorf("%s: page=%v", path, got)
		}
	}
	if page(hit{Method: "POST", Status: 200, Path: "/"}) || page(hit{Method: "GET", Status: 404, Path: "/"}) {
		t.Error("POST or 404 counted")
	}
}

type rtFunc func(*http.Request) (*http.Response, error)

func (f rtFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// list retries a 429 from entries.list and fails once the retries run out.
func TestLogViewListRetries429(t *testing.T) {
	logBackoff = time.Millisecond
	defer func() { logBackoff = 2 * time.Second }()
	reply := func(code int, body string) *http.Response {
		return &http.Response{StatusCode: code, Status: http.StatusText(code), Header: http.Header{},
			Body: io.NopCloser(strings.NewReader(body))}
	}
	for _, tc := range []struct {
		fails   int
		wantErr bool
	}{{2, false}, {logRetries + 1, true}} {
		calls := 0
		l := logView{name: "v", client: &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
			if r.URL.Host == "metadata.google.internal" {
				return reply(200, `{"access_token":"t"}`), nil
			}
			calls++
			if calls <= tc.fails {
				return reply(429, `{}`), nil
			}
			return reply(200, `{"entries":[{},{}]}`), nil
		})}}
		n := 0
		err := l.list(context.Background(), "", "entries(timestamp)", 1, func(json.RawMessage) error { n++; return nil })
		if tc.wantErr {
			if err == nil || calls != logRetries+1 {
				t.Errorf("fails=%d: err=%v calls=%d, want an error after %d calls", tc.fails, err, calls, logRetries+1)
			}
			continue
		}
		if err != nil || n != 2 || calls != tc.fails+1 {
			t.Errorf("fails=%d: err=%v entries=%d calls=%d", tc.fails, err, n, calls)
		}
	}
}

// A read that fails partway keeps its hits; the next load resumes from the
// last one (less readOverlap), and later loads read only what's new.
func TestTrafficResumes(t *testing.T) {
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, pacific)
	at := func(h int) time.Time { return now.Add(time.Duration(-h) * time.Hour) }
	all := []hit{
		{At: at(50), IP: "1.1.1.1", UA: chrome, Path: "/a", Method: "GET", Status: 200},
		{At: at(40), IP: "1.1.1.2", UA: chrome, Path: "/b", Method: "GET", Status: 200},
		{At: at(30), IP: "1.1.1.3", UA: chrome, Path: "/c", Method: "GET", Status: 200},
	}
	var starts []time.Time
	fail := true
	tr := &traffic{host: "site.example", now: func() time.Time { return now },
		home: func(context.Context) map[string]bool { return nil },
		hits: func(_ context.Context, since time.Time) ([]hit, error) {
			starts = append(starts, since)
			var out []hit
			for _, x := range all {
				if !x.At.Before(since) {
					out = append(out, x)
				}
			}
			if fail {
				fail = false
				return out[:1], fmt.Errorf("entries.list: 429 Too Many Requests")
			}
			return out, nil
		}}
	ctx := context.Background()
	if _, _, err := tr.recent(ctx, false); !errors.As(err, new(errReading)) {
		t.Fatalf("first load: err = %v, want errReading", err)
	}
	hits, _, err := tr.recent(ctx, false)
	if err != nil || len(hits) != 3 {
		t.Fatalf("second load: %d hits, err %v", len(hits), err)
	}
	if want := at(50).Add(-readOverlap); !starts[1].Equal(want) {
		t.Errorf("resumed at %v, want %v", starts[1], want)
	}
	all = append(all, hit{At: at(1), IP: "1.1.1.4", UA: chrome, Path: "/d", Method: "GET", Status: 200})
	resume := at(30).Add(-readOverlap)
	now = now.Add(2 * time.Hour)
	hits, _, err = tr.recent(ctx, false)
	if err != nil || len(hits) != 4 {
		t.Fatalf("hourly load: %d hits, err %v", len(hits), err)
	}
	if !starts[2].Equal(resume) {
		t.Errorf("hourly load read from %v, want %v", starts[2], resume)
	}
}
