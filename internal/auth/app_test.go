package auth

import (
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestAppHandoff(t *testing.T) {
	a := testAuth(t)
	mux := http.NewServeMux()
	a.Routes(mux)
	verifier := "a-verifier-the-app-keeps-to-itself-0123456789"
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	start := "/auth/app?challenge=" + challenge

	// Signed out: to the login page, coming back here.
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", start, nil))
	if loc := w.Header().Get("Location"); w.Code != http.StatusFound || loc != "/auth/login?next="+url.QueryEscape(start) {
		t.Fatalf("signed out: %d %q", w.Code, loc)
	}

	// No challenge: refused.
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/auth/app", nil))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("no challenge: %d", w.Code)
	}

	// Signed in: a code to the app.
	req := httptest.NewRequest("GET", start, nil)
	req.AddCookie(a.SessionCookie("nick@example.com"))
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	loc, _ := url.Parse(w.Header().Get("Location"))
	if w.Code != http.StatusFound || !strings.HasPrefix(loc.String(), AppCallback+"?code=") {
		t.Fatalf("signed in: %d %q", w.Code, loc)
	}
	code := loc.Query().Get("code")

	redeem := func(code, verifier string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest("GET", "/auth/app/redeem?next=%2Fwww%2F&code="+url.QueryEscape(code)+"&verifier="+url.QueryEscape(verifier), nil))
		return w
	}
	if w := redeem(code, "wrong"); w.Code != http.StatusUnauthorized {
		t.Fatalf("wrong verifier: %d", w.Code)
	}
	if w := redeem(code+"0", verifier); w.Code != http.StatusUnauthorized {
		t.Fatalf("tampered: %d", w.Code)
	}
	// A session cookie isn't a code.
	if w := redeem(a.SessionCookie("nick@example.com").Value, verifier); w.Code != http.StatusUnauthorized {
		t.Fatalf("session as code: %d", w.Code)
	}
	w = redeem(code, verifier)
	if w.Code != http.StatusFound || w.Header().Get("Location") != "/www/" {
		t.Fatalf("redeem: %d %q", w.Code, w.Header().Get("Location"))
	}
	cookies := w.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("cookies: %v", cookies)
	}
	if login, _, ok := a.verify(cookies[0].Value, time.Now()); !ok || login != "nick@example.com" {
		t.Fatalf("session: %q %v", login, ok)
	}

	// Expired.
	old := a.appCode("nick@example.com", challenge, time.Now().Add(-time.Second))
	if _, ok := a.redeemAppCode(old, verifier, time.Now()); ok {
		t.Fatal("expired code redeemed")
	}
}
