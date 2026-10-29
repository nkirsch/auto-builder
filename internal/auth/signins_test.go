package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nkirsch/auto-builder/internal/db"
)

// A single board records each callback's outcome.
func TestSignInsRecorded(t *testing.T) {
	ctx := context.Background()
	a, st := dbAuth(t)
	st.UpsertMember(ctx, "katrina", "member", "test")
	st.SetMemberEmail(ctx, "katrina", "katrina@example.com")
	st.UpsertMember(ctx, "octocat", "member", "test")
	google := func(email string, verified bool) {
		srv := fakeProvider(t, map[string]any{"email": email, "email_verified": verified})
		a.google.Endpoint.TokenURL = srv.URL + "/token"
		a.userinfo = srv.URL + "/userinfo"
		callback(a.handleGoogleCallback, "/auth/google/callback")
	}
	github := func(login string) {
		srv := fakeProvider(t, map[string]string{"login": login})
		a.oauth.Endpoint.TokenURL = srv.URL + "/token"
		a.api = srv.URL
		callback(a.handleCallback, "/auth/callback")
	}
	google("Katrina@Example.com", true)
	google("stranger@example.com", true)
	google("unverified@example.com", false)
	github("octocat")
	github("katrina")
	github("someone")
	r := httptest.NewRequest(http.MethodGet, "/auth/callback?state=nope&code=c", nil)
	r.AddCookie(&http.Cookie{Name: stateCookie, Value: "abc|/"})
	r.Header.Set("X-Forwarded-For", "203.0.113.9, 10.0.0.1")
	r.Header.Set("User-Agent", "Mozilla/5.0")
	a.handleCallback(httptest.NewRecorder(), r)

	got, err := st.SignIns(ctx, "", nil, false, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, s := range got {
		lines = append(lines, s.Provider+" "+s.Identity+": "+s.Result)
	}
	want := []string{
		"github : state mismatch", "github someone: not a member", "github katrina: Google only", "github octocat: ok",
		"google unverified@example.com: unverified email", "google stranger@example.com: not a member", "google katrina@example.com: ok",
	}
	if strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Fatalf("sign-ins:\n%s\nwant:\n%s", strings.Join(lines, "\n"), strings.Join(want, "\n"))
	}
	if got[0].IP != "203.0.113.9" || got[0].UserAgent != "Mozilla/5.0" {
		t.Errorf("ip %q ua %q", got[0].IP, got[0].UserAgent)
	}

	// A member's own attempts, or failures matching them.
	mine, _ := st.SignIns(ctx, "", []string{"Katrina", "katrina@example.com"}, true, time.Now().Add(-time.Hour))
	if len(mine) != 2 {
		t.Errorf("katrina's attempts: %+v", mine)
	}
	if err := st.PruneSignIns(ctx, time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if left, _ := st.SignIns(ctx, "", nil, false, time.Time{}); len(left) != 0 {
		t.Errorf("not pruned: %d", len(left))
	}
}

type fakeSignIns struct{ got []db.SignIn }

func (f *fakeSignIns) RecordSignIn(_ context.Context, s db.SignIn, within time.Duration) error {
	for _, o := range f.got {
		if within > 0 && o.Identity == s.Identity && o.Result == s.Result && o.Board == s.Board {
			return nil
		}
	}
	f.got = append(f.got, s)
	return nil
}

func (f *fakeSignIns) SignIns(context.Context, string, []string, bool, time.Time) ([]db.SignIn, error) {
	return f.got, nil
}

func (f *fakeSignIns) PruneSignIns(context.Context, time.Time) error { return nil }

// On the hub, a board records whether a fresh session is a member, once;
// callback failures name the board from next.
func TestMultiSignInsRecorded(t *testing.T) {
	ctx := context.Background()
	_, st := dbAuth(t)
	st.UpsertMember(ctx, "katrina", "member", "test")
	st.SetMemberEmail(ctx, "katrina", "katrina@example.com")
	log := &fakeSignIns{}
	a, err := New(Config{BaseURL: "https://hub.example", SessionKey: []byte("0123456789abcdef"),
		Multi: true, SignIns: log, BoardOf: func(next string) string {
			slug, _, _ := strings.Cut(strings.TrimPrefix(next, "/"), "/")
			return slug
		}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	h := a.ForTenant(st, "/alpha").Require(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	visit := func(c *http.Cookie) {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.AddCookie(c)
		h.ServeHTTP(httptest.NewRecorder(), r)
	}
	stale := &http.Cookie{Name: hostCookie, Value: a.sign("old@example.com", time.Now().Add(sessionTTL-time.Hour))}
	for _, c := range []*http.Cookie{a.SessionCookie("katrina@example.com"), a.SessionCookie("katrina@example.com"),
		a.SessionCookie("stranger@example.com"), a.SessionCookie("katrina"), stale} {
		visit(c)
	}
	srv := fakeProvider(t, map[string]any{"email": "x@example.com", "email_verified": false})
	a.google.Endpoint.TokenURL = srv.URL + "/token"
	a.userinfo = srv.URL + "/userinfo"
	r := httptest.NewRequest(http.MethodGet, "/auth/google/callback?state=abc&code=c", nil)
	r.AddCookie(&http.Cookie{Name: stateCookie, Value: "abc|/alpha/items/1"})
	a.handleGoogleCallback(httptest.NewRecorder(), r)

	var lines []string
	for _, s := range log.got {
		lines = append(lines, s.Board+" "+s.Provider+" "+s.Identity+": "+s.Result)
	}
	want := []string{
		"alpha google katrina@example.com: ok", "alpha google stranger@example.com: not a member",
		"alpha github katrina: Google only", "alpha google x@example.com: unverified email",
	}
	if strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Fatalf("sign-ins:\n%s\nwant:\n%s", strings.Join(lines, "\n"), strings.Join(want, "\n"))
	}
}
