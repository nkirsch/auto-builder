package auth

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func testAuth(t *testing.T) *Auth {
	a, err := New(Config{BaseURL: "https://builder.example", SessionKey: []byte("0123456789abcdef0123")}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestSessionRoundTrip(t *testing.T) {
	a := testAuth(t)
	now := time.Now()
	v := a.sign("octocat", now.Add(time.Hour))
	if login, ok := a.verify(v, now); !ok || login != "octocat" {
		t.Fatalf("verify = %q, %v", login, ok)
	}
	if _, ok := a.verify(v, now.Add(2*time.Hour)); ok {
		t.Fatal("expired session accepted")
	}
	forged := a.sign("admin", now.Add(time.Hour))
	payload, _, _ := strings.Cut(forged, ".")
	_, sig, _ := strings.Cut(v, ".")
	if _, ok := a.verify(payload+"."+sig, now); ok {
		t.Fatal("payload with another session's signature accepted")
	}
	other := testAuth(t)
	other.cfg.SessionKey = []byte("another-key-another-key")
	if _, ok := other.verify(v, now); ok {
		t.Fatal("session from another key accepted")
	}
}

func TestSessionKeyRequiredOffLocalhost(t *testing.T) {
	if _, err := New(Config{BaseURL: "https://builder.example"}, nil); err == nil {
		t.Fatal("missing SESSION_KEY accepted")
	}
	if _, err := New(Config{BaseURL: "http://localhost:8080"}, nil); err != nil {
		t.Fatal(err)
	}
}

func TestDevLoginOnlyOnLocalhost(t *testing.T) {
	a, _ := New(Config{BaseURL: "https://builder.example", SessionKey: []byte("0123456789abcdef"), DevLogin: "x"}, nil)
	if a.dev {
		t.Fatal("DEV_LOGIN honoured off localhost")
	}
	a, _ = New(Config{BaseURL: "http://localhost:8080", DevLogin: "x"}, nil)
	if !a.dev {
		t.Fatal("DEV_LOGIN ignored on localhost")
	}
}

func TestSafeNext(t *testing.T) {
	for in, want := range map[string]string{
		"/items/3": "/items/3", "": "/", "//evil.com": "/", "https://evil.com": "/", "/\\evil.com": "/",
	} {
		if got := safeNext(in); got != want {
			t.Errorf("safeNext(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSameOrigin(t *testing.T) {
	a := testAuth(t)
	cases := []struct {
		origin, site string
		want         bool
	}{
		{"https://builder.example", "", true},
		{"https://evil.example", "", false},
		{"", "same-origin", true},
		{"", "cross-site", false},
		{"", "", true},
	}
	for _, c := range cases {
		r := httptest.NewRequest(http.MethodPost, "/items", nil)
		if c.origin != "" {
			r.Header.Set("Origin", c.origin)
		}
		if c.site != "" {
			r.Header.Set("Sec-Fetch-Site", c.site)
		}
		if got := a.sameOrigin(r); got != c.want {
			t.Errorf("origin=%q site=%q: got %v", c.origin, c.site, got)
		}
	}
}

func TestBearer(t *testing.T) {
	h := Bearer("s3cret", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	for hdr, want := range map[string]int{"Bearer s3cret": 200, "Bearer nope": 401, "": 401, "s3cret": 401} {
		r := httptest.NewRequest(http.MethodPost, "/", nil)
		r.Header.Set("Authorization", hdr)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != want {
			t.Errorf("%q: %d, want %d", hdr, w.Code, want)
		}
	}
	empty := Bearer("", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	r.Header.Set("Authorization", "Bearer ")
	w := httptest.NewRecorder()
	empty.ServeHTTP(w, r)
	if w.Code != 401 {
		t.Error("empty token accepted")
	}
}

func TestRequireRedirectsSignedOutGetAndHead(t *testing.T) {
	a := testAuth(t)
	h := a.Require(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	for _, m := range []string{http.MethodGet, http.MethodHead} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(m, "/items/3", nil))
		if w.Code != http.StatusFound || w.Header().Get("Location") != "/auth/login?next=%2Fitems%2F3" {
			t.Errorf("%s: %d %q", m, w.Code, w.Header().Get("Location"))
		}
	}
}
