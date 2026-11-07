package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nkirsch/auto-builder/internal/db"
	"github.com/nkirsch/auto-builder/internal/gate"
	"github.com/nkirsch/auto-builder/internal/github"
	"github.com/nkirsch/auto-builder/internal/tenant"
)

func TestVersionLabels(t *testing.T) {
	d := func(day int) time.Time { return time.Date(2026, 10, day, 12, 0, 0, 0, time.UTC) }
	commits := []github.Commit{{SHA: "ccc3333"}, {SHA: "bbb2222"}, {SHA: "aaa1111"}, {SHA: "0000000"}} // newest first
	tags := map[string][]string{"aaa1111": {"v26.10.20-1"}, "ccc3333": {"v26.11.1-1"}}
	prs := []github.MergedPR{
		{SHA: "aaa1111", Title: "Halloween theme"},
		{SHA: "bbb2222", Title: "Thanksgiving transition"},
		{SHA: "ccc3333", Title: "Fix menu"},
	}
	revs := []gate.Revision{
		{Name: "x-a-00003", Created: d(30), SHA: "ccc3333"},
		{Name: "x-a-00001", Created: d(20), SHA: "aaa1111"},
		{Name: "x-a-00002", Created: d(21), SHA: "aaa1111"}, // a redeploy: same commit
		{Name: "x-a-00000", Created: d(1)},                  // the placeholder, no GIT_SHA
	}
	got := versionLabels(revs, commits, tags, prs)
	want := map[string]struct {
		label   string
		changes string
	}{
		"x-a-00000": {"x-a-00000", ""},
		"x-a-00001": {"v26.10.20-1", "Halloween theme"},
		"x-a-00002": {"v26.10.20-1", ""},
		"x-a-00003": {"v26.11.1-1", "Fix menu|Thanksgiving transition"},
	}
	for rev, w := range want {
		v := got[rev]
		if v.Label != w.label || strings.Join(v.Changes, "|") != w.changes {
			t.Errorf("%s: %q %q, want %q %q", rev, v.Label, v.Changes, w.label, w.changes)
		}
	}
	// Untagged: the short SHA.
	if v := versionLabels([]gate.Revision{{Name: "r", SHA: "bbb2222ffff"}}, nil, nil, nil)["r"]; v.Label != "bbb2222" {
		t.Errorf("untagged label %q", v.Label)
	}
}

func TestBuildVisitors(t *testing.T) {
	at := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	get := func(ip, ua, path, ref, rev string, sec int) hit {
		return hit{At: at.Add(time.Duration(sec) * time.Second), IP: ip, UA: ua, Path: path, Ref: ref,
			Method: "GET", Status: 200, Revision: rev}
	}
	var hits []hit
	hits = append(hits,
		get("1.1.1.1", chrome, "/", "https://www.google.com/", "r1", 0),
		get("1.1.1.1", chrome, "/a.css", "", "r1", 1),
		get("1.1.1.1", chrome, "/", "https://extreme.org/delta/", "r2", 3600),
		get("1.1.1.1", chrome, "/", "https://echo.extreme.org/", "r2", 3700),
		get("2.2.2.2", chrome, "/", "", "r1", 0), // no assets
		get("3.3.3.3", "GPTBot/1.0", "/", "", "r1", 0),
		get("4.4.4.4", chrome, "/", "", "r1", 0), // an admin
		get("4.4.4.4", chrome, "/a.css", "", "r1", 0),
		get("5.5.5.5", chrome, "/a.css", "", "r1", 0),
	)
	for i := range visitorBurst { // crawler: a burst of pages
		hits = append(hits, get("5.5.5.5", chrome, "/p/", "", "r1", i))
	}
	labels := map[string]version{"r1": {Label: "v26.10.20-1", Changes: []string{"Halloween theme"}}, "r2": {Label: "v26.11.1-1"}}
	apps := []appHits{{App: "delta", Host: "delta.extreme.org", Hits: hits, Exclude: map[string]bool{"4.4.4.4": true}, Labels: labels}}
	h := &hub{host: "extreme.org", cfg: config{BaseURL: "https://extreme.org"}}
	key := []byte("k")
	r := buildVisitors(apps, key, "delta", h.hiddenReferrer)
	if r.Total != 1 || r.Views != 3 || r.Dropped != 3+visitorBurst {
		t.Fatalf("total=%d views=%d dropped=%d", r.Total, r.Views, r.Dropped)
	}
	x := r.Rows[0]
	if x.ID != pseudonym(key, "delta", "1.1.1.1", chrome) || x.Browser != "Chrome" {
		t.Errorf("row %+v", x)
	}
	if strings.Join(x.Versions, ",") != "v26.10.20-1,v26.11.1-1" || strings.Join(x.Referrers, ",") != "www.google.com" {
		t.Errorf("versions=%v referrers=%v", x.Versions, x.Referrers)
	}
	if len(r.Versions) != 2 || r.Versions[0].Label == "" || r.Versions[0].Visitors != 1 {
		t.Errorf("versions %+v", r.Versions)
	}
	// IDs are scoped: the same browser differs per board and on the platform view.
	if pseudonym(key, "delta", "1.1.1.1", chrome) == pseudonym(key, "echo", "1.1.1.1", chrome) ||
		pseudonym(key, "delta", "1.1.1.1", chrome) == pseudonym(key, "*", "1.1.1.1", chrome) {
		t.Error("pseudonyms not scoped")
	}
}

func TestHiddenReferrer(t *testing.T) {
	h := &hub{host: "extreme.org", cfg: config{BaseURL: "https://extreme.org"}}
	for host, hidden := range map[string]bool{
		"extreme.org": true, "echo.extreme.org": true, "www.google.com": false, "notextreme.org": false, "extreme.org.evil.com": false,
	} {
		if got := h.hiddenReferrer(host); got != hidden {
			t.Errorf("%s: hidden=%v", host, got)
		}
	}
}

func TestVisitorsHost(t *testing.T) {
	h := &hub{host: "extreme.org", cfg: config{BaseURL: "https://extreme.org"}}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(299) })
	for _, c := range []struct {
		host, path string
		code       int
		loc        string
	}{
		{"visitors.extreme.org", "/", 302, "https://extreme.org/visitors"},
		{"visitors.extreme.org", "/anything", 302, "https://extreme.org/visitors"},
		{"visitors.extreme.org", "/health", 299, ""},
		{"extreme.org", "/", 299, ""},
	} {
		r := httptest.NewRequest("GET", c.path, nil)
		r.Host = c.host
		w := httptest.NewRecorder()
		h.visitorsHost(next).ServeHTTP(w, r)
		if w.Code != c.code || w.Header().Get("Location") != c.loc {
			t.Errorf("%s%s: %d %q", c.host, c.path, w.Code, w.Header().Get("Location"))
		}
	}
}

func TestVisitorsReserved(t *testing.T) {
	if err := tenant.ValidSlug("visitors"); err == nil {
		t.Error("visitors is a hub path and host; it must be reserved")
	}
}

// A board created before its slug was reserved still serves under /<slug>/,
// while /<slug> stays the hub's page.
func TestSinceReservedBoard(t *testing.T) {
	h := testHub(t)
	ctx := context.Background()
	if _, err := h.reg.Control.Exec(ctx, `INSERT INTO tenants (slug, name, repo, created_by, existing, status, provisioned_at)
		VALUES ('visitors', 'Visitors', 'o/visitors', 'nkirsch', true, 'ready', now())`); err != nil {
		t.Fatal(err)
	}
	if _, err := h.reg.Store(ctx, "visitors"); err != nil {
		t.Fatal(err)
	}
	srv := h.routes()
	if w := do(h, srv, "nick@example.org", "GET", "/visitors/", nil); w.Code == http.StatusNotFound {
		t.Errorf("board: %d %.200s", w.Code, w.Body.String())
	}
	if w := do(h, srv, "nick@example.org", "GET", "/visitors", nil); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Visitors") {
		t.Errorf("hub page: %d %.200s", w.Code, w.Body.String())
	}
}

// TestHubVisitorsPage: platform admins see every app; a board admin only
// theirs; nobody sees IPs, user agents, or admins' own visits.
func TestHubVisitorsPage(t *testing.T) {
	ctx := context.Background()
	h := testHub(t)
	h.cfg.LogProject = "proj"
	for _, slug := range []string{"alpha", "beta"} {
		if err := h.reg.Create(ctx, tenant.Tenant{Slug: slug, Repo: "o/" + slug, CreatedBy: "nkirsch", Existing: true}); err != nil {
			t.Fatal(err)
		}
		if err := h.reg.SetStatus(ctx, slug, "ready", ""); err != nil {
			t.Fatal(err)
		}
	}
	alphaSt, _ := h.reg.Store(ctx, "alpha")
	if err := alphaSt.UpsertMember(ctx, "ann", "admin", "nkirsch"); err != nil {
		t.Fatal(err)
	}
	if err := alphaSt.UpsertMember(ctx, "bob", "member", "nkirsch"); err != nil {
		t.Fatal(err)
	}
	// ann's sign-in IP is an admin's: left out.
	if err := h.reg.RecordSignIn(ctx, db.SignIn{Provider: "github", Identity: "ann", Result: db.SignInOK, Board: "alpha", IP: "9.9.9.9"}, 0); err != nil {
		t.Fatal(err)
	}
	at := time.Now().Add(-time.Hour)
	const svc = "projects/proj/locations/r/services/x-alpha"
	// alpha's revision x-r1 runs a tagged release; beta's stays unlabeled.
	h.versioners = map[string]*versioner{"alpha " + svc: {
		revisions: func(context.Context) ([]gate.Revision, error) {
			return []gate.Revision{{Name: "x-r1", SHA: "aaa1111", Created: at.Add(-time.Hour)}}, nil
		},
		history: func(context.Context) ([]github.Commit, map[string][]string, []github.MergedPR, error) {
			return []github.Commit{{SHA: "aaa1111"}}, map[string][]string{"aaa1111": {"v26.10.31-1"}},
				[]github.MergedPR{{SHA: "aaa1111", Title: "Halloween theme"}}, nil
		},
	}, "beta " + svc: {revisions: func(context.Context) ([]gate.Revision, error) { return nil, nil }}}
	const firefox = "Mozilla/5.0 (Windows NT 10.0) Gecko/20100101 Firefox/140.0"
	view := func(ip, ua, path, ref string) []hit {
		return []hit{{At: at, IP: ip, UA: ua, Path: path, Ref: ref, Method: "GET", Status: 200, Revision: "x-r1", Service: svc},
			{At: at, IP: ip, UA: ua, Path: "/s.css", Method: "GET", Status: 200}}
	}
	alphaHits := append(view("1.1.1.1", chrome, "/halloween/", "https://beta.hub.example/"), view("9.9.9.9", firefox, "/secret-admin-page/", "")...)
	for slug, hs := range map[string][]hit{"alpha": alphaHits, "beta": view("2.2.2.2", firefox, "/beta-page/", "")} {
		tr := h.traffic(slug)
		tr.home = func(context.Context) map[string]bool { return nil }
		tr.hits = func(context.Context, time.Time) ([]hit, error) { return hs, nil }
	}

	srv := h.routes()
	never := []string{"1.1.1.1", "2.2.2.2", "9.9.9.9", chrome, firefox, "beta.hub.example", "secret-admin-page", "ann"}
	clean := func(who, path string, want, notWant []string) {
		t.Helper()
		w := do(h, srv, who, "GET", path, nil)
		body := w.Body.String()
		if w.Code != http.StatusOK {
			t.Fatalf("%s %s: %d %s", who, path, w.Code, body)
		}
		// The layout names the signed-in viewer; the report must not name anyone.
		report := body[strings.Index(body, "<h1>Visitors</h1>"):]
		for _, s := range want {
			if !strings.Contains(report, s) {
				t.Errorf("%s %s: missing %q:\n%s", who, path, s, report)
			}
		}
		for _, s := range append(never, notWant...) {
			if strings.Contains(report, s) {
				t.Errorf("%s %s: leaks %q:\n%s", who, path, s, report)
			}
		}
	}
	clean("nkirsch", "/visitors", []string{"/halloween/", "/beta-page/", "<b>2</b> visitors", "v26.10.31-1", "Halloween theme", "x-r1"}, nil)
	clean("ann", "/alpha/visitors", []string{"/halloween/", "<b>1</b> visitors", "alpha.hub.example", "<b>v26.10.31-1</b>", "Halloween theme"}, []string{"beta"})
	// ann admins one app: /visitors sends her there.
	if w := do(h, srv, "ann", "GET", "/visitors", nil); w.Code != http.StatusFound || w.Header().Get("Location") != "/alpha/visitors" {
		t.Errorf("ann /visitors: %d %q", w.Code, w.Header().Get("Location"))
	}
	for _, c := range []struct{ who, path string }{{"bob", "/alpha/visitors"}, {"bob", "/visitors"}, {"ann", "/beta/visitors"}, {"mallory", "/visitors"}} {
		if w := do(h, srv, c.who, "GET", c.path, nil); w.Code != http.StatusForbidden {
			t.Errorf("%s %s: %d, want 403", c.who, c.path, w.Code)
		}
	}
}
