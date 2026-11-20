// Package hubauth signs people into an app on an auto-builder hub through
// the hub's own sign-in (TENANCY.md "App sign-in"), so the app needs no
// OAuth client of its own. Standard library only.
//
//	a := hubauth.New("https://www.extreme.org") // issuer https://auth.extreme.org, client "www"
//	mux.HandleFunc("GET /auth/login", func(w, r) { a.Login(w, r, "/") })
//	mux.HandleFunc("GET /auth/callback", func(w, r) {
//		id, next, err := a.Callback(w, r)
//		// check id.Email, start the app's own session, redirect to next
//	})
package hubauth

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Identity is who signed in.
type Identity struct {
	Subject string `json:"sub"`   // the hub's identity: a lower-cased Google address or a GitHub login
	Email   string `json:"email"` // set for Google sign-ins, which are verified
	Login   string `json:"login"` // set for GitHub sign-ins
	Name    string `json:"name"`
	Picture string `json:"picture"`
}

// Client signs people into one app.
type Client struct {
	Issuer   string // https://auth.<domain>
	ClientID string // the app's slug
	SiteURL  string // the app's origin, where /auth/callback is
	HTTP     *http.Client
	Now      func() time.Time

	mu     sync.Mutex
	keys   map[string]ed25519.PublicKey
	keysAt time.Time
}

// stateCookie carries state, nonce, PKCE verifier and where to go after.
const stateCookie = "hubauth_state"

// New derives the issuer and client ID from the app's URL:
// https://<slug>.<domain> signs in through https://auth.<domain> as <slug>.
func New(siteURL string) *Client {
	siteURL = strings.TrimRight(siteURL, "/")
	c := &Client{SiteURL: siteURL, HTTP: &http.Client{Timeout: 10 * time.Second}, Now: time.Now}
	if u, err := url.Parse(siteURL); err == nil {
		if slug, domain, ok := strings.Cut(u.Hostname(), "."); ok {
			c.ClientID, c.Issuer = slug, "https://auth."+domain
		}
	}
	return c
}

func (c *Client) redirectURI() string { return c.SiteURL + "/auth/callback" }

func random() string {
	b := make([]byte, 32)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// Login sends the browser to the hub; next is a path on this app to return to.
func (c *Client) Login(w http.ResponseWriter, r *http.Request, next string) {
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") {
		next = "/"
	}
	state, nonce, verifier := random(), random(), random()
	v, _ := json.Marshal([]string{state, nonce, verifier, next})
	http.SetCookie(w, &http.Cookie{Name: stateCookie, Value: base64.RawURLEncoding.EncodeToString(v), Path: "/auth/",
		MaxAge: 600, HttpOnly: true, Secure: strings.HasPrefix(c.SiteURL, "https:"), SameSite: http.SameSiteLaxMode})
	sum := sha256.Sum256([]byte(verifier))
	q := url.Values{"response_type": {"code"}, "client_id": {c.ClientID}, "redirect_uri": {c.redirectURI()},
		"scope": {"openid email profile"}, "state": {state}, "nonce": {nonce},
		"code_challenge": {base64.RawURLEncoding.EncodeToString(sum[:])}, "code_challenge_method": {"S256"}}
	http.Redirect(w, r, c.Issuer+"/authorize?"+q.Encode(), http.StatusFound)
}

// Callback finishes sign-in at /auth/callback: it checks state, trades the
// code for an ID token and verifies it. next is where Login was asked to return.
func (c *Client) Callback(w http.ResponseWriter, r *http.Request) (id Identity, next string, err error) {
	ck, err := r.Cookie(stateCookie)
	if err != nil {
		return id, "", errors.New("sign-in expired; try again")
	}
	http.SetCookie(w, &http.Cookie{Name: stateCookie, Path: "/auth/", MaxAge: -1})
	raw, _ := base64.RawURLEncoding.DecodeString(ck.Value)
	var st []string
	if json.Unmarshal(raw, &st) != nil || len(st) != 4 {
		return id, "", errors.New("bad sign-in state")
	}
	q := r.URL.Query()
	if q.Get("state") != st[0] {
		return id, "", errors.New("sign-in state doesn't match")
	}
	if e := q.Get("error"); e != "" {
		return id, "", fmt.Errorf("hub refused sign-in: %s", e)
	}
	form := url.Values{"grant_type": {"authorization_code"}, "code": {q.Get("code")}, "client_id": {c.ClientID},
		"redirect_uri": {c.redirectURI()}, "code_verifier": {st[2]}}
	resp, err := c.HTTP.PostForm(c.Issuer+"/token", form)
	if err != nil {
		return id, "", err
	}
	defer resp.Body.Close()
	var tok struct {
		IDToken string `json:"id_token"`
		Error   string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tok); err != nil || resp.StatusCode != http.StatusOK {
		return id, "", fmt.Errorf("token exchange: %s %s", resp.Status, tok.Error)
	}
	claims, err := c.Verify(r.Context(), tok.IDToken)
	if err != nil {
		return id, "", err
	}
	if claims["nonce"] != st[1] {
		return id, "", errors.New("nonce doesn't match")
	}
	b, _ := json.Marshal(claims)
	json.Unmarshal(b, &id)
	return id, st[3], nil
}

// Verify checks an ID token's signature (the issuer's JWKS), issuer,
// audience and expiry, and returns its claims.
func (c *Client) Verify(ctx context.Context, token string) (map[string]any, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, errors.New("malformed ID token")
	}
	hb, err1 := base64.RawURLEncoding.DecodeString(parts[0])
	sig, err2 := base64.RawURLEncoding.DecodeString(parts[2])
	body, err3 := base64.RawURLEncoding.DecodeString(parts[1])
	if err := errors.Join(err1, err2, err3); err != nil {
		return nil, err
	}
	var head struct{ Alg, Kid string }
	if json.Unmarshal(hb, &head) != nil || head.Alg != "EdDSA" {
		return nil, errors.New("unexpected ID token algorithm")
	}
	key, err := c.key(ctx, head.Kid)
	if err != nil {
		return nil, err
	}
	if !ed25519.Verify(key, []byte(parts[0]+"."+parts[1]), sig) {
		return nil, errors.New("bad ID token signature")
	}
	var claims map[string]any
	if err := json.Unmarshal(body, &claims); err != nil {
		return nil, err
	}
	exp, _ := claims["exp"].(float64)
	switch {
	case claims["iss"] != c.Issuer:
		return nil, errors.New("ID token from another issuer")
	case claims["aud"] != c.ClientID:
		return nil, errors.New("ID token for another app")
	case int64(exp) < c.Now().Unix():
		return nil, errors.New("ID token expired")
	}
	return claims, nil
}

// key returns the issuer's signing key kid, refetching the JWKS at most once a minute.
func (c *Client) key(ctx context.Context, kid string) (ed25519.PublicKey, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if k, ok := c.keys[kid]; ok {
		return k, nil
	}
	if c.Now().Sub(c.keysAt) < time.Minute {
		return nil, errors.New("unknown ID token key")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.Issuer+"/jwks", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var set struct {
		Keys []struct{ Kty, Crv, X, Kid string }
	}
	if err := json.NewDecoder(resp.Body).Decode(&set); err != nil {
		return nil, fmt.Errorf("jwks: %w", err)
	}
	c.keys, c.keysAt = map[string]ed25519.PublicKey{}, c.Now()
	for _, k := range set.Keys {
		if x, err := base64.RawURLEncoding.DecodeString(k.X); err == nil && k.Kty == "OKP" && k.Crv == "Ed25519" && len(x) == ed25519.PublicKeySize {
			c.keys[k.Kid] = x
		}
	}
	if k, ok := c.keys[kid]; ok {
		return k, nil
	}
	return nil, errors.New("unknown ID token key")
}
