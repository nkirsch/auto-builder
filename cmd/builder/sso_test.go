package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/nkirsch/auto-builder/hubauth"
	"github.com/nkirsch/auto-builder/internal/tenant"
)

// handlerTransport sends a client's requests straight to a handler.
type handlerTransport struct{ h http.Handler }

func (t handlerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r.Host = r.URL.Host
	w := httptest.NewRecorder()
	t.h.ServeHTTP(w, r)
	return w.Result(), nil
}

func TestAppSignIn(t *testing.T) {
	ctx := context.Background()
	h := testHub(t)
	srv := h.routes()
	if err := h.reg.Create(ctx, tenant.Tenant{Slug: "www", Repo: "kirsch/www", CreatedBy: "nkirsch", Existing: true}); err != nil {
		t.Fatal(err)
	}
	// The issuer is auth.<APP_DOMAIN>; the app derives it and its client ID from its own URL.
	app := hubauth.New("https://www.hub.example")
	app.HTTP = &http.Client{Transport: handlerTransport{srv}}
	if app.Issuer != "https://auth.hub.example" || app.ClientID != "www" {
		t.Fatalf("client %s %s", app.Issuer, app.ClientID)
	}
	get := func(identity, rawURL string) *httptest.ResponseRecorder {
		u, _ := url.Parse(rawURL)
		r := httptest.NewRequest("GET", rawURL, nil)
		r.Host = u.Host
		if identity != "" {
			r.AddCookie(h.auth.SessionCookie(identity))
		}
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, r)
		return w
	}

	w := get("", "https://auth.hub.example/.well-known/openid-configuration")
	var disc map[string]any
	json.Unmarshal(w.Body.Bytes(), &disc)
	if disc["issuer"] != "https://auth.hub.example" || disc["authorization_endpoint"] != "https://auth.hub.example/authorize" {
		t.Fatalf("discovery %s", w.Body)
	}

	// Login → the issuer's /authorize → the hub's /sso/authorize, where the session is.
	login := httptest.NewRecorder()
	app.Login(login, httptest.NewRequest("GET", "https://www.hub.example/auth/login", nil), "/members/")
	state := login.Result().Cookies()[0]
	w = get("", login.Header().Get("Location"))
	authorize := w.Header().Get("Location")
	if w.Code != 302 || !strings.HasPrefix(authorize, "https://hub.example/sso/authorize?") {
		t.Fatalf("issuer /authorize: %d %s", w.Code, authorize)
	}
	// Signed out, the hub signs them in first.
	if w := get("", authorize); w.Code != 302 || !strings.HasPrefix(w.Header().Get("Location"), "/auth/login?next=%2Fsso%2Fauthorize") {
		t.Fatalf("signed out: %d %s", w.Code, w.Header().Get("Location"))
	}
	w = get("nick@example.org", authorize)
	back, _ := url.Parse(w.Header().Get("Location"))
	if w.Code != 302 || back.Host != "www.hub.example" || back.Path != "/auth/callback" || back.Query().Get("code") == "" {
		t.Fatalf("authorize: %d %s", w.Code, back)
	}

	cb := httptest.NewRequest("GET", back.String(), nil)
	cb.AddCookie(state)
	id, next, err := app.Callback(httptest.NewRecorder(), cb)
	if err != nil || id.Email != "nick@example.org" || id.Subject != "nick@example.org" || next != "/members/" {
		t.Fatalf("callback %+v %q %v", id, next, err)
	}
	// A code works once.
	if _, _, err := app.Callback(httptest.NewRecorder(), cb); err == nil || !strings.Contains(err.Error(), "invalid_grant") {
		t.Fatalf("reused code: %v", err)
	}

	// Codes only go back to the app's own host, and only for a live app.
	q := url.Values{"client_id": {"www"}, "redirect_uri": {"https://evil.example/auth/callback"}, "response_type": {"code"},
		"code_challenge": {strings.Repeat("a", 43)}, "code_challenge_method": {"S256"}}
	for name, mod := range map[string]func(url.Values){
		"other host":     func(v url.Values) {},
		"sibling app":    func(v url.Values) { v.Set("redirect_uri", "https://jane.hub.example/auth/callback") },
		"http":           func(v url.Values) { v.Set("redirect_uri", "http://www.hub.example/auth/callback") },
		"unknown client": func(v url.Values) { v.Set("client_id", "nope"); v.Set("redirect_uri", "https://nope.hub.example/x") },
	} {
		v := url.Values{}
		for k, x := range q {
			v[k] = x
		}
		mod(v)
		if w := get("nick@example.org", "https://hub.example/sso/authorize?"+v.Encode()); w.Code != 400 {
			t.Errorf("%s: %d %s", name, w.Code, w.Header().Get("Location"))
		}
	}
	// Without PKCE the error goes back to the (good) redirect.
	q.Set("redirect_uri", "https://www.hub.example/auth/callback")
	q.Del("code_challenge")
	if w := get("nick@example.org", "https://hub.example/sso/authorize?"+q.Encode()); !strings.Contains(w.Header().Get("Location"), "error=invalid_request") {
		t.Errorf("no PKCE: %d %s", w.Code, w.Header().Get("Location"))
	}

	// A GitHub sign-in has a login and no email; userinfo takes the token as bearer.
	tok, err := signJWT(h.ssoKey, h.ssoClaims(ctx, "www", "nkirsch", "", app.Now()))
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("GET", "https://auth.hub.example/userinfo", nil)
	r.Host = "auth.hub.example"
	r.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, r)
	var info map[string]any
	json.Unmarshal(rec.Body.Bytes(), &info)
	if rec.Code != 200 || info["login"] != "nkirsch" || info["email"] != nil {
		t.Fatalf("userinfo %d %s", rec.Code, rec.Body)
	}
	r.Header.Set("Authorization", "Bearer "+tok[:len(tok)-2]+"xx")
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, r)
	if rec.Code != 401 {
		t.Fatalf("tampered userinfo %d", rec.Code)
	}
	// Anything else on the auth host goes to the hub.
	if w := get("", "https://auth.hub.example/admin"); w.Code != 302 || w.Header().Get("Location") != "https://hub.example/" {
		t.Fatalf("auth host other path: %d %s", w.Code, w.Header().Get("Location"))
	}
}
