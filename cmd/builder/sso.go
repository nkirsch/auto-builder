package main

import (
	"context"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/nkirsch/auto-builder/internal/db"
	"github.com/nkirsch/auto-builder/internal/tenant"
)

// App sign-in (TENANCY.md "App sign-in"): the hub is an OpenID Connect
// provider for its own apps at auth.<APP_DOMAIN>. An app sends people to
// /authorize, the hub signs them in with its own sign-in (its one Google
// callback) and returns a one-time code to the app's own host, which the app
// trades at /token, with its PKCE verifier, for an ID token. The client ID is
// the app's slug; there's no secret and no consent page.

// ssoTokenTTL is how long an ID token is good for: apps keep their own sessions.
const ssoTokenTTL = 5 * time.Minute

// ssoKey derives the hub's Ed25519 signing key from SESSION_KEY, so rotating
// one rotates the other.
func ssoKey(sessionKey string) ed25519.PrivateKey {
	mac := hmac.New(sha256.New, []byte(sessionKey))
	mac.Write([]byte("sso signing key"))
	return ed25519.NewKeyFromSeed(mac.Sum(nil))
}

func ssoKid(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return hex.EncodeToString(sum[:8])
}

// authHostName is the issuer's host: auth.<APP_DOMAIN>.
func (h *hub) authHostName() string { return "auth." + h.host }

// issuer is the apps' OpenID Connect issuer.
func (h *hub) issuer() string { return "https://" + h.authHostName() }

// authHost serves the issuer's endpoints on auth.<APP_DOMAIN>; anything else
// there goes to the hub.
func (h *hub) authHost(next http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", h.ssoDiscovery)
	mux.HandleFunc("GET /jwks", h.ssoJWKS)
	mux.HandleFunc("POST /token", h.ssoToken)
	mux.HandleFunc("GET /userinfo", h.ssoUserinfo)
	mux.HandleFunc("GET /authorize", func(w http.ResponseWriter, r *http.Request) {
		// The session cookie lives on the hub's host.
		http.Redirect(w, r, h.cfg.BaseURL+"/sso/authorize?"+r.URL.RawQuery, http.StatusFound)
	})
	mux.Handle("GET /health", next)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, h.cfg.BaseURL+"/", http.StatusFound)
	})
	host := h.authHostName()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host == host {
			mux.ServeHTTP(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (h *hub) ssoDiscovery(w http.ResponseWriter, _ *http.Request) {
	iss := h.issuer()
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	writeJSON(w, http.StatusOK, map[string]any{
		"issuer":                                iss,
		"authorization_endpoint":                iss + "/authorize",
		"token_endpoint":                        iss + "/token",
		"userinfo_endpoint":                     iss + "/userinfo",
		"jwks_uri":                              iss + "/jwks",
		"response_types_supported":              []string{"code"},
		"grant_types_supported":                 []string{"authorization_code"},
		"subject_types_supported":               []string{"public"},
		"id_token_signing_alg_values_supported": []string{"EdDSA"},
		"scopes_supported":                      []string{"openid", "email", "profile"},
		"token_endpoint_auth_methods_supported": []string{"none"},
		"code_challenge_methods_supported":      []string{"S256"},
		"claims_supported":                      []string{"iss", "aud", "sub", "exp", "iat", "nonce", "email", "email_verified", "login", "name", "picture"},
	})
}

func (h *hub) ssoJWKS(w http.ResponseWriter, _ *http.Request) {
	pub := h.ssoKey.Public().(ed25519.PublicKey)
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	writeJSON(w, http.StatusOK, map[string]any{"keys": []map[string]string{{
		"kty": "OKP", "crv": "Ed25519", "x": base64.RawURLEncoding.EncodeToString(pub),
		"kid": ssoKid(pub), "alg": "EdDSA", "use": "sig",
	}}})
}

// ssoRedirect checks redirect_uri: https on the app's own host, nothing
// that could carry the code elsewhere.
func (h *hub) ssoRedirect(slug, redirect string) bool {
	u, err := url.Parse(redirect)
	if err != nil || u.Fragment != "" || u.User != nil || !validRedirect(redirect) {
		return false
	}
	app, err := url.Parse(h.appURL(slug))
	return err == nil && u.Scheme == app.Scheme && u.Host == app.Host
}

// ssoAuthorize signs the person in (signedIn) and sends a code back to the app.
func (h *hub) ssoAuthorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	slug, redirect := q.Get("client_id"), q.Get("redirect_uri")
	t, err := h.reg.Tenant(r.Context(), slug)
	if err != nil || !tenant.WellFormed(slug) || !t.Live() {
		http.Error(w, "unknown app", http.StatusBadRequest)
		return
	}
	if !h.ssoRedirect(slug, redirect) {
		http.Error(w, "redirect_uri must be on "+h.appURL(slug), http.StatusBadRequest)
		return
	}
	req := oauthRequest{redirect: redirect, state: q.Get("state")}
	challenge := q.Get("code_challenge")
	switch {
	case q.Get("response_type") != "code":
		oauthBack(w, r, req, "unsupported_response_type")
		return
	case len(challenge) != 43 || q.Get("code_challenge_method") != "S256":
		oauthBack(w, r, req, "invalid_request") // PKCE S256 is required
		return
	case len(q.Get("nonce")) > 200:
		oauthBack(w, r, req, "invalid_request")
		return
	}
	me := h.auth.SignedIn(r)
	code, err := h.reg.IssueSSOCode(r.Context(), tenant.SSOCode{Client: slug, Identity: me, RedirectURI: redirect,
		Challenge: challenge, Nonce: q.Get("nonce")})
	if err != nil {
		slog.Error("sso code", "err", err)
		oauthBack(w, r, req, "server_error")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	oauthRedirect(w, r, req, url.Values{"code": {code}, "iss": {h.issuer()}})
}

// ssoClaims are the ID token's claims for identity at app aud.
func (h *hub) ssoClaims(ctx context.Context, aud, identity, nonce string, now time.Time) map[string]any {
	c := map[string]any{"iss": h.issuer(), "aud": aud, "sub": identity, "iat": now.Unix(), "exp": now.Add(ssoTokenTTL).Unix()}
	if strings.Contains(identity, "@") {
		c["email"], c["email_verified"] = identity, true // Google sign-in addresses are verified
	} else {
		c["login"] = identity // a GitHub login
	}
	if nonce != "" {
		c["nonce"] = nonce
	}
	if name, avatar, err := h.reg.User(ctx, identity); err == nil {
		if name != "" {
			c["name"] = name
		}
		if avatar != "" {
			c["picture"] = avatar
		}
	}
	return c
}

// signJWT makes a compact EdDSA JWT.
func signJWT(key ed25519.PrivateKey, claims map[string]any) (string, error) {
	head, _ := json.Marshal(map[string]string{"alg": "EdDSA", "typ": "JWT", "kid": ssoKid(key.Public().(ed25519.PublicKey))})
	body, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	in := base64.RawURLEncoding.EncodeToString(head) + "." + base64.RawURLEncoding.EncodeToString(body)
	return in + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, []byte(in))), nil
}

// verifyJWT checks a token this hub signed and returns its claims.
func verifyJWT(key ed25519.PrivateKey, token string, now time.Time) (map[string]any, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, errors.New("malformed token")
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || !ed25519.Verify(key.Public().(ed25519.PublicKey), []byte(parts[0]+"."+parts[1]), sig) {
		return nil, errors.New("bad signature")
	}
	body, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, err
	}
	var c map[string]any
	if err := json.Unmarshal(body, &c); err != nil {
		return nil, err
	}
	if exp, _ := c["exp"].(float64); int64(exp) < now.Unix() {
		return nil, errors.New("expired")
	}
	return c, nil
}

func (h *hub) ssoToken(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if err := r.ParseForm(); err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_request", "bad form")
		return
	}
	if r.PostFormValue("grant_type") != "authorization_code" {
		oauthError(w, http.StatusBadRequest, "unsupported_grant_type", "only authorization_code")
		return
	}
	c, err := h.reg.TakeSSOCode(r.Context(), r.PostFormValue("code"))
	if err != nil || c.Client != r.PostFormValue("client_id") || c.RedirectURI != r.PostFormValue("redirect_uri") ||
		!pkceOK(r.PostFormValue("code_verifier"), c.Challenge) {
		if err != nil && !errors.Is(err, db.ErrNotFound) {
			slog.Error("sso code", "err", err)
		}
		oauthError(w, http.StatusBadRequest, "invalid_grant", "bad or spent code")
		return
	}
	tok, err := signJWT(h.ssoKey, h.ssoClaims(r.Context(), c.Client, c.Identity, c.Nonce, time.Now()))
	if err != nil {
		slog.Error("sso token", "err", err)
		oauthError(w, http.StatusInternalServerError, "server_error", "signing failed")
		return
	}
	slog.Info("app sign-in", "app", c.Client, "identity", c.Identity)
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"id_token": tok, "access_token": tok, "token_type": "Bearer",
		"expires_in": int(ssoTokenTTL.Seconds())})
}

// ssoUserinfo answers with an ID token's claims, given it as the bearer.
func (h *hub) ssoUserinfo(w http.ResponseWriter, r *http.Request) {
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	c, err := verifyJWT(h.ssoKey, tok, time.Now())
	if !ok || err != nil || c["iss"] != h.issuer() {
		w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	for _, k := range []string{"iss", "aud", "exp", "iat", "nonce"} {
		delete(c, k)
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, c)
}
