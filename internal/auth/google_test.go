package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/nkirsch/auto-builder/internal/db"
)

// dbAuth needs TEST_DATABASE_URL (a disposable database; members are truncated).
func dbAuth(t *testing.T) (*Auth, *db.Store) {
	t.Helper()
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
	if _, err := pool.Exec(ctx, `TRUNCATE members CASCADE`); err != nil {
		t.Fatal(err)
	}
	st := &db.Store{Pool: pool}
	a, err := New(Config{BaseURL: "https://builder.example", SessionKey: []byte("0123456789abcdef0123"),
		ClientID: "gh", GoogleClientID: "goog", GoogleClientSecret: "x"}, st)
	if err != nil {
		t.Fatal(err)
	}
	return a, st
}

// fakeProvider serves a token endpoint and a user endpoint returning user.
func fakeProvider(t *testing.T, user any) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/token" {
			json.NewEncoder(w).Encode(map[string]string{"access_token": "tok", "token_type": "Bearer"})
			return
		}
		if r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, "no token", http.StatusUnauthorized)
			return
		}
		json.NewEncoder(w).Encode(user)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// callback runs a provider callback with a matching state cookie.
func callback(h http.HandlerFunc, path string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, path+"?state=abc&code=c", nil)
	r.AddCookie(&http.Cookie{Name: stateCookie, Value: "abc|/items/1"})
	w := httptest.NewRecorder()
	h(w, r)
	return w
}

func sessionLogin(a *Auth, w *httptest.ResponseRecorder) string {
	for _, c := range w.Result().Cookies() {
		if c.Name == sessionCookie {
			login, _ := a.verify(c.Value, time.Now())
			return login
		}
	}
	return ""
}

func TestGoogleSignInByEmail(t *testing.T) {
	ctx := context.Background()
	a, st := dbAuth(t)
	st.UpsertMember(ctx, "katrina", "member", "test")
	if err := st.SetMemberEmail(ctx, "katrina", "Katrina@example.com"); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		email    string
		verified bool
		code     int
		login    string
	}{
		{"katrina@example.com", true, http.StatusFound, "katrina"},
		{"katrina@example.com", false, http.StatusBadGateway, ""},
		{"stranger@example.com", true, http.StatusForbidden, ""},
	} {
		srv := fakeProvider(t, map[string]any{"email": c.email, "email_verified": c.verified,
			"name": "Katrina", "picture": "https://lh3.googleusercontent.com/a/x=s96-c"})
		a.google.Endpoint.TokenURL = srv.URL + "/token"
		a.userinfo = srv.URL + "/userinfo"
		w := callback(a.handleGoogleCallback, "/auth/google/callback")
		if w.Code != c.code || sessionLogin(a, w) != c.login {
			t.Errorf("%s verified=%v: %d login %q, want %d %q", c.email, c.verified, w.Code, sessionLogin(a, w), c.code, c.login)
		}
		if c.login != "" && w.Header().Get("Location") != "/items/1" {
			t.Errorf("redirect %q", w.Header().Get("Location"))
		}
	}
	m, _ := st.Member(ctx, "katrina")
	if m.Name != "Katrina" || m.LastSeen == nil {
		t.Errorf("member not touched: %+v", m)
	}
}

// A member with an email signs in with GitHub only when marked GitHub too.
func TestGitHubSignInRefusedForEmailMember(t *testing.T) {
	ctx := context.Background()
	a, st := dbAuth(t)
	st.UpsertMember(ctx, "katrina", "member", "test")
	st.SetMemberEmail(ctx, "katrina", "katrina@example.com")
	st.UpsertMember(ctx, "octocat", "member", "test")
	// nkirsch's login is his GitHub account too, so either works.
	st.UpsertMember(ctx, "nkirsch", "admin", "test")
	st.SetMemberEmail(ctx, "nkirsch", "nick@example.com")
	st.SetMemberGitHub(ctx, "nkirsch", true)
	for login, want := range map[string]int{"katrina": http.StatusForbidden, "octocat": http.StatusFound, "nkirsch": http.StatusFound} {
		srv := fakeProvider(t, map[string]string{"login": login})
		a.oauth.Endpoint.TokenURL = srv.URL + "/token"
		a.api = srv.URL
		w := callback(a.handleCallback, "/auth/callback")
		if w.Code != want {
			t.Errorf("%s: %d, want %d", login, w.Code, want)
		}
		if want == http.StatusForbidden && sessionLogin(a, w) != "" {
			t.Errorf("%s: session issued", login)
		}
	}
}

func TestProvidersOffered(t *testing.T) {
	a, _ := New(Config{BaseURL: "https://builder.example", SessionKey: []byte("0123456789abcdef"), GoogleClientID: "g"}, nil)
	if a.GitHub() || !a.Google() {
		t.Fatalf("GitHub=%v Google=%v", a.GitHub(), a.Google())
	}
	w := httptest.NewRecorder()
	a.handleGoogle(w, httptest.NewRequest(http.MethodGet, "/auth/google?next=/x", nil))
	if loc := w.Header().Get("Location"); w.Code != http.StatusFound || !strings.Contains(loc, "accounts.google.com") ||
		!strings.Contains(loc, "scope=openid+email+profile") {
		t.Fatalf("%d %q", w.Code, loc)
	}
}
