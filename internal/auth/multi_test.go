package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

type fakeUsers map[string]string

func (u fakeUsers) SetUser(_ context.Context, identity, name, _ string) error {
	u[identity] = name
	return nil
}

func (u fakeUsers) User(_ context.Context, identity string) (string, string, error) {
	return u[identity], "", nil
}

// Multi mode: sign-in records who you are without checking any board, and
// the session is a host-only __Host- cookie.
func TestMultiSignIn(t *testing.T) {
	users := fakeUsers{}
	a, err := New(Config{BaseURL: "https://hub.example", SessionKey: []byte("0123456789abcdef"),
		ClientID: "gh", GoogleClientID: "g", Multi: true, Users: users}, nil)
	if err != nil {
		t.Fatal(err)
	}
	gh := fakeProvider(t, map[string]string{"login": "octocat", "name": "Octo"})
	a.oauth.Endpoint.TokenURL = gh.URL + "/token"
	a.api = gh.URL
	goog := fakeProvider(t, map[string]any{"email": "Kat@Example.com", "email_verified": true, "name": "Kat"})
	a.google.Endpoint.TokenURL = goog.URL + "/token"
	a.userinfo = goog.URL + "/userinfo"

	for path, want := range map[string]string{"/auth/callback": "octocat", "/auth/google/callback": "kat@example.com"} {
		h := a.handleCallback
		if strings.Contains(path, "google") {
			h = a.handleGoogleCallback
		}
		w := callback(h, path)
		var got *http.Cookie
		for _, c := range w.Result().Cookies() {
			if c.Name == hostCookie {
				got = c
			}
		}
		if w.Code != http.StatusFound || got == nil || got.Path != "/" || got.Domain != "" || !got.Secure {
			t.Fatalf("%s: %d cookie %+v", path, w.Code, got)
		}
		if id, _ := a.verify(got.Value, time.Now()); id != want {
			t.Errorf("%s: identity %q, want %q", path, id, want)
		}
	}
	if users["octocat"] != "Octo" || users["kat@example.com"] != "Kat" {
		t.Errorf("users %v", users)
	}

	// A board's sign-in start sends the hub's callback back into the board.
	ta := a.ForTenant(nil, "/alpha")
	w := httptest.NewRecorder()
	ta.handleGoogle(w, httptest.NewRequest(http.MethodGet, "/auth/google?next=/items/3", nil))
	c := w.Result().Cookies()[0]
	if c.Name != stateCookie || c.Path != "/auth" || !strings.HasSuffix(c.Value, "|/alpha/items/3") {
		t.Errorf("state cookie %+v", c)
	}
	if !strings.Contains(w.Header().Get("Location"), "redirect_uri=https%3A%2F%2Fhub.example%2Fauth%2Fgoogle%2Fcallback") {
		t.Errorf("redirect %q", w.Header().Get("Location"))
	}
}

// A board resolves the session's identity: an address by member email, a
// login by member login unless that member signs in with Google.
func TestMultiRequire(t *testing.T) {
	ctx := context.Background()
	_, st := dbAuth(t)
	st.UpsertMember(ctx, "katrina", "member", "test")
	st.SetMemberEmail(ctx, "katrina", "katrina@example.com")
	st.UpsertMember(ctx, "octocat", "member", "test")
	a, err := New(Config{BaseURL: "https://hub.example", SessionKey: []byte("0123456789abcdef"),
		Multi: true, Users: fakeUsers{"octocat": "Octo"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ta := a.ForTenant(st, "/alpha")
	h := ta.Require(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(Member(r.Context()).Login))
	}))
	for identity, want := range map[string]string{
		"katrina@example.com": "200 katrina", "KATRINA@example.com": "200 katrina",
		"octocat": "200 octocat", "katrina": "403", "stranger": "403", "": "302",
	} {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		if identity != "" {
			r.AddCookie(a.SessionCookie(identity))
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		got := strconv.Itoa(w.Code)
		if w.Code == http.StatusOK {
			got += " " + w.Body.String()
		}
		if got != want {
			t.Errorf("%q: %s, want %s", identity, got, want)
		}
	}
	if m, _ := st.Member(ctx, "octocat"); m.Name != "Octo" || m.LastSeen == nil {
		t.Errorf("member not touched from the profile: %+v", m)
	}
}
