package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nkirsch/auto-builder/internal/db"
	"github.com/nkirsch/auto-builder/internal/github"
)

// /admin/hubs puts every hub's urgent, escalated and blocked items first,
// counts the rest, and lists each hub's boards; hubs read each other's
// /api/overview with STATS_TOKEN.
func TestHubOverview(t *testing.T) {
	ctx := context.Background()
	h := testHub(t)
	h.launcher, h.provisioner = &fakeJob{}, &fakeJob{}
	h.cfg.StatsToken = "tok"
	srv := h.routes()
	if w := do(h, srv, "nkirsch", "POST", "/new", url.Values{"slug": {"alpha"}, "name": {"Alpha"}}); w.Code != 303 {
		t.Fatalf("create: %d", w.Code)
	}
	st, _ := h.reg.Store(ctx, "alpha")
	urgent, _ := st.CreateItem(ctx, "Site down", "", "nkirsch")
	st.Escalate(ctx, urgent, "platform", "nkirsch", nil)
	blocked, _ := st.CreateItem(ctx, "Needs a secret", "", "nkirsch")
	st.SetBlocked(ctx, blocked, "Add the FOO secret.", false)
	done, _ := st.CreateItem(ctx, "Closed thing", "", "nkirsch")
	st.CloseItem(ctx, done, "done", true)
	// One run done, then two failed: failing, and 2 failed in 7d.
	for _, status := range []string{"done", "failed", "failed"} {
		run, err := st.CreateRun(ctx, "tick", "")
		if err != nil {
			t.Fatal(err)
		}
		st.FinishRun(ctx, run, status, "", "", nil)
	}
	// The platform repo's master is 4 commits past the live GIT_SHA.
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/kirsch/extreme/commits/HEAD":
			w.Write([]byte(`{"sha":"head9"}`))
		case "/repos/kirsch/extreme/compare/abc1234...head9":
			w.Write([]byte(`{"ahead_by":4}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer gh.Close()
	h.gh = github.New(gh.URL, "")
	// alpha's request log view holds 5 server errors in the last day.
	h.cfg.LogProject = "logs"
	var views []string
	h.countErrs = func(_ context.Context, view string, since time.Time) (int, error) {
		views = append(views, view)
		if time.Since(since) < 23*time.Hour {
			t.Errorf("5xx counted since %v", since)
		}
		return 5, nil
	}

	// This hub's overview, as another hub reads it.
	r := httptest.NewRequest("GET", "/api/overview", nil)
	w := httptest.NewRecorder()
	if srv.ServeHTTP(w, r); w.Code != http.StatusUnauthorized {
		t.Errorf("overview without token: %d", w.Code)
	}
	r.Header.Set("Authorization", "Bearer tok")
	w = httptest.NewRecorder()
	srv.ServeHTTP(w, r)
	var o hubOverview
	if err := json.NewDecoder(w.Body).Decode(&o); err != nil || len(o.Sites) != 1 {
		t.Fatalf("overview: %d %v %+v", w.Code, err, o)
	}
	s := o.Sites[0]
	if s.Name != "Alpha" || s.Board != "https://hub.example/alpha/" || !s.App || len(s.Stats.Attention) != 2 || s.Stats.Other != 1 {
		t.Errorf("alpha overview %+v", s)
	}
	if !o.ReleaseKnown || o.Unreleased != 4 || !s.ErrorsKnown || s.Errors24h != 5 || s.Stats.Failed7d != 2 || s.Stats.FailStreak != 2 {
		t.Errorf("overview release lag, 5xx or failed runs: %+v", o)
	}
	if len(views) != 1 || views[0] != "projects/logs/locations/global/buckets/_Default/views/alpha-requests" {
		t.Errorf("5xx read from %v", views)
	}

	// Another hub that answers, and one that doesn't take our token.
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		json.NewEncoder(w).Encode(hubOverview{Name: "ignored", Upstream: "nkirsch/auto-builder", Behind: 3, Sites: []overviewSite{{
			Name: "Maple", Board: "https://hub.example.net/maple/", App: true, Health: "ok", Detail: true, Releases: true, Unreleased: 2,
			Stats: siteStats{Other: 4, Attention: []attention{{ID: 7, Title: "Escalated thing", Author: "juniper", Kind: "escalated"}}},
		}}})
	}))
	defer other.Close()
	locked := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer locked.Close()
	h.cfg.Hubs = []remoteSite{{Name: "kirsch", URL: other.URL}, {Name: "nick", URL: locked.URL}, {Name: "self", URL: "https://hub.example"}}

	body := do(h, srv, "nkirsch", "GET", "/admin/hubs", nil).Body.String()
	iu, ie, ib, ifl := strings.Index(body, "Site down"), strings.Index(body, "Escalated thing"), strings.Index(body, "Needs a secret"),
		strings.Index(body, "Last 2 runs failed")
	if iu < 0 || ie < iu || ib < ie || ifl < ib {
		t.Errorf("attention not urgent, escalated, blocked, failing in order (%d %d %d %d): %s", iu, ie, ib, ifl, body)
	}
	for _, want := range []string{
		`<a href="https://hub.example/alpha/items/` + strconv.Itoa(urgent) + `"><b>Site down</b></a>`,
		`<a href="https://hub.example.net/maple/items/7">`,
		"Add the FOO secret.",
		"5 other unfinished items",
		`<a href="` + other.URL + `/">kirsch</a>`,
		"3 commits behind nkirsch/auto-builder",
		`<span class="behind">2 unreleased</span>`,
		"HTTP 401: does it have this hub&#39;s STATS_TOKEN?",
		`<a href="https://hub.example/alpha/runs"><b>Last 2 runs failed</b></a>`,
		`<span class="behind">4 unreleased</span>`,
		`<a class="bad" href="https://hub.example/alpha/logs">5</a>`,
		`<span class="bad">2 failed in 7d</span>`,
		`<a href="` + other.URL + `/admin/hubs">Release all (1) →</a>`, // another hub releases its own apps
	} {
		if !strings.Contains(body, want) {
			t.Errorf("hubs page missing %s", want)
		}
	}
	if strings.Contains(body, "Closed thing") || strings.Count(body, `<a href="https://hub.example/">hub.example</a>`) != 1 {
		t.Errorf("hubs page shows a shipped item or this hub twice: %s", body)
	}
	// kirsch's hub predates these fields: its release lag and 5xx show as –.
	kirsch := body[strings.Index(body, `">kirsch</a>`):]
	kirsch = kirsch[:strings.Index(kirsch, "</table>")]
	if !strings.Contains(kirsch, `</a> <span class="muted">–</span></h2>`) || strings.Count(kirsch, `<td class="num"><span class="muted">–</span></td>`) != 1 {
		t.Errorf("an older hub's missing fields aren't –: %s", kirsch)
	}
	if len(views) != 1 {
		t.Errorf("5xx count not cached: read %d times", len(views))
	}
	if w := do(h, srv, "juniper", "GET", "/admin/hubs", nil); w.Code != http.StatusForbidden {
		t.Errorf("hubs page for a non-platform-admin: %d", w.Code)
	}
	// alpha doesn't deploy on release: no button here, and Release all skips it.
	if strings.Contains(body, `action="/admin/hubs/release"`) {
		t.Errorf("Release all offered with nothing to release")
	}
	if w := do(h, srv, "nkirsch", "POST", "/admin/hubs/release", nil); w.Code != 303 || w.Header().Get("Location") != "/admin/hubs?flash=Nothing+to+release" {
		t.Errorf("release all: %d %q", w.Code, w.Header().Get("Location"))
	}
	if w := do(h, srv, "juniper", "POST", "/admin/hubs/release", nil); w.Code != http.StatusForbidden {
		t.Errorf("release all by a non-platform-admin: %d", w.Code)
	}
}

// AppsUnreleased counts the hub's own apps with unreleased commits.
func TestAppsUnreleased(t *testing.T) {
	o := hubOverview{Local: true, Sites: []overviewSite{{App: true, Releases: true, Unreleased: 3}, {App: true, Releases: true}, {Unreleased: 2}}}
	if n := o.AppsUnreleased(); n != 1 {
		t.Errorf("AppsUnreleased = %d, want 1", n)
	}
}

// A board is failing when its newest finished runs failed in a row; runs in
// flight or cancelled don't break the streak.
func TestFailStreak(t *testing.T) {
	runs := func(statuses ...string) []db.Run {
		var out []db.Run
		for _, s := range statuses {
			out = append(out, db.Run{Status: s})
		}
		return out
	}
	for _, c := range []struct {
		runs []db.Run
		want int
	}{
		{runs(), 0},
		{runs("done", "failed", "failed"), 0},
		{runs("failed", "done", "failed"), 1},
		{runs("running", "failed", "cancelled", "failed", "done"), 2},
		{runs("failed", "failed", "failed"), 3},
	} {
		if got := failStreak(c.runs); got != c.want {
			t.Errorf("failStreak(%v) = %d, want %d", c.runs, got, c.want)
		}
	}
}

// A successful run clears the failed runs before it from failed_7d.
func TestFailedClearedByDone(t *testing.T) {
	ctx := context.Background()
	h := testHub(t)
	if w := do(h, h.routes(), "nkirsch", "POST", "/new", url.Values{"slug": {"alpha"}, "name": {"Alpha"}}); w.Code != 303 {
		t.Fatalf("create: %d", w.Code)
	}
	st, _ := h.reg.Store(ctx, "alpha")
	finish := func(statuses ...string) siteStats {
		for _, status := range statuses {
			run, err := st.CreateRun(ctx, "tick", "")
			if err != nil {
				t.Fatal(err)
			}
			st.FinishRun(ctx, run, status, "", "", nil)
		}
		s, err := statsFor(ctx, st, "v", "sha")
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	if s := finish("failed", "failed"); s.Failed7d != 2 {
		t.Errorf("before done: failed_7d %d, want 2", s.Failed7d)
	}
	if s := finish("done"); s.Failed7d != 0 || s.LastDone == nil {
		t.Errorf("after done: failed_7d %d last_done %v", s.Failed7d, s.LastDone)
	}
	if s := finish("failed"); s.Failed7d != 1 {
		t.Errorf("failed after done: failed_7d %d, want 1", s.Failed7d)
	}
}

// lastDone is the newest successful run's finish time, else its creation time.
func TestLastDone(t *testing.T) {
	t1, t2 := time.Unix(100, 0), time.Unix(200, 0)
	if got := lastDone([]db.Run{{Status: "failed"}}); got != nil {
		t.Errorf("no done run: got %v", got)
	}
	got := lastDone([]db.Run{{Status: "failed", FinishedAt: &t2}, {Status: "done", CreatedAt: t1, FinishedAt: &t2}, {Status: "done", CreatedAt: t1}})
	if got == nil || !got.Equal(t2) {
		t.Errorf("got %v, want %v", got, t2)
	}
	if got := lastDone([]db.Run{{Status: "done", CreatedAt: t1}}); got == nil || !got.Equal(t1) {
		t.Errorf("unfinished done: got %v, want %v", got, t1)
	}
}

// /api/signins lists successful sign-ins at every board, behind STATS_TOKEN.
func TestAPISignIns(t *testing.T) {
	ctx := context.Background()
	h := testHub(t)
	h.cfg.StatsToken = "tok"
	srv := h.routes()
	for _, s := range []db.SignIn{
		{Provider: "google", Identity: "pat@example.com", Result: db.SignInOK, Board: "alpha", IP: "1.2.3.4", UserAgent: "Safari"},
		{Provider: "google", Identity: "eve@example.com", Result: db.SignInNotMember, Board: "alpha", IP: "6.6.6.6", UserAgent: "x"},
	} {
		if err := h.reg.RecordSignIn(ctx, s, 0); err != nil {
			t.Fatal(err)
		}
	}
	get := func(token string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "/api/signins?days=7", nil)
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, r)
		return w
	}
	if w := get(""); w.Code != http.StatusUnauthorized {
		t.Errorf("no token: %d", w.Code)
	}
	w := get("tok")
	var out []struct{ Identity, Board, IP, UA string }
	if err := json.NewDecoder(w.Body).Decode(&out); err != nil || w.Code != http.StatusOK {
		t.Fatalf("%d %v", w.Code, err)
	}
	if len(out) != 1 || out[0].Identity != "pat@example.com" || out[0].Board != "alpha" || out[0].IP != "1.2.3.4" || out[0].UA != "Safari" {
		t.Errorf("signins: %+v", out)
	}
}

// A signed-in session's later IPs + browsers join /api/signins, once each.
func TestAPISignInsSeen(t *testing.T) {
	h := testHub(t)
	h.cfg.StatsToken = "tok"
	srv := h.routes()
	for range 2 {
		r := httptest.NewRequest("GET", "/mine", nil)
		r.AddCookie(h.auth.SessionCookie("pat@example.com"))
		r.Header.Set("X-Forwarded-For", "9.9.9.9")
		r.Header.Set("User-Agent", "Phone")
		srv.ServeHTTP(httptest.NewRecorder(), r)
	}
	r := httptest.NewRequest("GET", "/static/style.css", nil)
	r.AddCookie(h.auth.SessionCookie("pat@example.com"))
	r.Header.Set("X-Forwarded-For", "8.8.8.8")
	srv.ServeHTTP(httptest.NewRecorder(), r)

	r = httptest.NewRequest("GET", "/api/signins?days=7", nil)
	r.Header.Set("Authorization", "Bearer tok")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, r)
	var out []struct{ Identity, IP, UA string }
	if err := json.NewDecoder(w.Body).Decode(&out); err != nil || w.Code != http.StatusOK {
		t.Fatalf("%d %v", w.Code, err)
	}
	if len(out) != 1 || out[0].Identity != "pat@example.com" || out[0].IP != "9.9.9.9" || out[0].UA != "Phone" {
		t.Errorf("signins: %+v", out)
	}
}
