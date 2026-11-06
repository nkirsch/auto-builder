package main

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/nkirsch/auto-builder/internal/db"
	"github.com/nkirsch/auto-builder/internal/tenant"
)

// OAuth 2.1 for /mcp: the hub is its own authorization server, and its normal
// sign-in decides who you are. Clients are public (PKCE, no secret) and
// register themselves, so the consent page is what stops a stranger's client:
// it names the client and where it will return to.

func (h *hub) oauthRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /.well-known/oauth-protected-resource", h.oauthResource)
	mux.HandleFunc("GET /.well-known/oauth-protected-resource/mcp", h.oauthResource)
	mux.HandleFunc("GET /.well-known/oauth-authorization-server", h.oauthServer)
	mux.HandleFunc("POST /oauth/register", h.oauthRegister)
	mux.HandleFunc("GET /oauth/authorize", h.signedIn(h.oauthAuthorize))
	mux.HandleFunc("POST /oauth/authorize", h.signedIn(h.oauthDecide))
	mux.HandleFunc("POST /oauth/token", h.oauthToken)
}

func (h *hub) mcpResource() string { return h.cfg.BaseURL + "/mcp" }

func (h *hub) oauthResource(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"resource":                 h.mcpResource(),
		"authorization_servers":    []string{h.cfg.BaseURL},
		"bearer_methods_supported": []string{"header"},
		"scopes_supported":         []string{"mcp"},
	})
}

func (h *hub) oauthServer(w http.ResponseWriter, _ *http.Request) {
	base := h.cfg.BaseURL
	writeJSON(w, http.StatusOK, map[string]any{
		"issuer":                                base,
		"authorization_endpoint":                base + "/oauth/authorize",
		"token_endpoint":                        base + "/oauth/token",
		"registration_endpoint":                 base + "/oauth/register",
		"response_types_supported":              []string{"code"},
		"grant_types_supported":                 []string{"authorization_code", "refresh_token"},
		"code_challenge_methods_supported":      []string{"S256"},
		"token_endpoint_auth_methods_supported": []string{"none"},
		"scopes_supported":                      []string{"mcp"},
	})
}

// oauthChallenge is /mcp's 401, which tells a client where to sign in.
func (h *hub) oauthChallenge(w http.ResponseWriter, invalid bool) {
	v := `Bearer resource_metadata="` + h.cfg.BaseURL + `/.well-known/oauth-protected-resource/mcp"`
	if invalid {
		v += `, error="invalid_token"`
	}
	w.Header().Set("WWW-Authenticate", v)
	http.Error(w, "unauthorized", http.StatusUnauthorized)
}

// loopback reports whether u is on this machine, where a native app listens
// on a port of its choosing (RFC 8252).
func loopback(u *url.URL) bool {
	host := u.Hostname()
	return host == "localhost" || (net.ParseIP(host) != nil && net.ParseIP(host).IsLoopback())
}

// validRedirect accepts https, or http only on loopback; never a fragment or
// userinfo, which could smuggle the code somewhere else.
func validRedirect(s string) bool {
	u, err := url.Parse(s)
	if err != nil || u.Host == "" || u.Fragment != "" || u.User != nil {
		return false
	}
	return u.Scheme == "https" || (u.Scheme == "http" && loopback(u))
}

// redirectAllowed matches a redirect against those registered: exactly, except
// that a loopback one may use any port.
func redirectAllowed(registered []string, redirect string) bool {
	if !validRedirect(redirect) {
		return false
	}
	if slices.Contains(registered, redirect) {
		return true
	}
	u, err := url.Parse(redirect)
	if err != nil || !loopback(u) {
		return false
	}
	for _, s := range registered {
		v, err := url.Parse(s)
		if err == nil && loopback(v) && v.Scheme == u.Scheme && v.Hostname() == u.Hostname() && v.Path == u.Path && v.RawQuery == u.RawQuery {
			return true
		}
	}
	return false
}

func oauthError(w http.ResponseWriter, code int, name, desc string) {
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, code, map[string]string{"error": name, "error_description": desc})
}

// oauthRegister is dynamic client registration (RFC 7591), public clients only.
func (h *hub) oauthRegister(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name         string   `json:"client_name"`
		RedirectURIs []string `json:"redirect_uris"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&in); err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_client_metadata", "bad JSON")
		return
	}
	name := strings.TrimSpace(in.Name)
	if name == "" {
		name = "An unnamed app"
	}
	if len(name) > 100 {
		oauthError(w, http.StatusBadRequest, "invalid_client_metadata", "client_name is too long")
		return
	}
	if len(in.RedirectURIs) == 0 || len(in.RedirectURIs) > 5 {
		oauthError(w, http.StatusBadRequest, "invalid_redirect_uri", "want 1 to 5 redirect_uris")
		return
	}
	for _, u := range in.RedirectURIs {
		if len(u) > 500 || !validRedirect(u) {
			oauthError(w, http.StatusBadRequest, "invalid_redirect_uri", "redirect_uris must be https, or http on loopback")
			return
		}
	}
	id, err := h.reg.RegisterOAuthClient(r.Context(), name, in.RedirectURIs)
	if errors.Is(err, tenant.ErrOAuthFull) {
		oauthError(w, http.StatusServiceUnavailable, "temporarily_unavailable", "too many clients")
		return
	}
	if err != nil {
		slog.Error("oauth register", "err", err)
		oauthError(w, http.StatusInternalServerError, "server_error", "registration failed")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusCreated, map[string]any{
		"client_id": id, "client_name": name, "redirect_uris": in.RedirectURIs,
		"grant_types": []string{"authorization_code", "refresh_token"}, "response_types": []string{"code"},
		"token_endpoint_auth_method": "none",
	})
}

// oauthRequest is a checked authorize request. Until the client and redirect
// are known good nothing redirects, so a bad one can't bounce a user anywhere.
type oauthRequest struct {
	client                            tenant.OAuthClient
	redirect, state, challenge, resrc string
}

// parseOAuthRequest answers the failure itself and returns ok=false.
func (h *hub) parseOAuthRequest(w http.ResponseWriter, r *http.Request, q func(string) string) (oauthRequest, bool) {
	c, err := h.reg.OAuthClientByID(r.Context(), q("client_id"))
	if err != nil {
		http.Error(w, "unknown client", http.StatusBadRequest)
		return oauthRequest{}, false
	}
	req := oauthRequest{client: c, redirect: q("redirect_uri"), state: q("state"), challenge: q("code_challenge"), resrc: q("resource")}
	if !redirectAllowed(c.RedirectURIs, req.redirect) {
		http.Error(w, "redirect_uri isn't registered for this client", http.StatusBadRequest)
		return oauthRequest{}, false
	}
	switch {
	case q("response_type") != "code" && r.Method == http.MethodGet:
		oauthBack(w, r, req, "unsupported_response_type")
	case len(req.challenge) != 43 || q("code_challenge_method") != "S256":
		oauthBack(w, r, req, "invalid_request") // PKCE S256 is required
	case !h.resourceOK(req.resrc):
		oauthBack(w, r, req, "invalid_target")
	default:
		return req, true
	}
	return oauthRequest{}, false
}

// resourceOK accepts this hub's /mcp, or no resource from a client that omits it.
func (h *hub) resourceOK(resource string) bool {
	return resource == "" || strings.TrimSuffix(resource, "/") == h.mcpResource()
}

// oauthBack returns to the (already checked) client with an error.
func oauthBack(w http.ResponseWriter, r *http.Request, req oauthRequest, name string) {
	oauthRedirect(w, r, req, url.Values{"error": {name}})
}

func oauthRedirect(w http.ResponseWriter, r *http.Request, req oauthRequest, params url.Values) {
	u, err := url.Parse(req.redirect)
	if err != nil { // validRedirect parsed it
		http.Error(w, "bad redirect_uri", http.StatusBadRequest)
		return
	}
	q := u.Query()
	for k, v := range params {
		q[k] = v
	}
	if req.state != "" {
		q.Set("state", req.state)
	}
	u.RawQuery = q.Encode()
	http.Redirect(w, r, u.String(), http.StatusFound)
}

func (h *hub) oauthAuthorize(w http.ResponseWriter, r *http.Request) {
	req, ok := h.parseOAuthRequest(w, r, r.URL.Query().Get)
	if !ok {
		return
	}
	// Consent in a frame could be clicked through without being seen.
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Content-Security-Policy", "frame-ancestors 'none'")
	w.Header().Set("Cache-Control", "no-store")
	u, _ := url.Parse(req.redirect)
	h.render(w, r, "oauth", "Connect an app", map[string]any{
		"Client": req.client.Name, "ClientID": req.client.ID, "RedirectURI": req.redirect, "RedirectHost": u.Hostname(),
		"State": req.state, "Challenge": req.challenge, "Resource": req.resrc,
	})
}

// oauthDecide is the consent form's answer. Everything is checked again: the
// hidden fields are the browser's to change.
func (h *hub) oauthDecide(w http.ResponseWriter, r *http.Request) {
	if !h.auth.SameOrigin(r) {
		http.Error(w, "cross-origin request refused", http.StatusForbidden)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	req, ok := h.parseOAuthRequest(w, r, r.PostFormValue)
	if !ok {
		return
	}
	if r.PostFormValue("decision") != "allow" {
		oauthBack(w, r, req, "access_denied")
		return
	}
	me := h.auth.SignedIn(r)
	code, err := h.reg.IssueOAuthCode(r.Context(), tenant.OAuthCode{ClientID: req.client.ID, Identity: me, RedirectURI: req.redirect, Challenge: req.challenge})
	if err != nil {
		slog.Error("oauth code", "err", err)
		oauthBack(w, r, req, "server_error")
		return
	}
	slog.Info("oauth consent", "identity", me, "client", req.client.ID, "name", req.client.Name, "redirect", req.redirect)
	oauthRedirect(w, r, req, url.Values{"code": {code}})
}

func pkceOK(verifier, challenge string) bool {
	if len(verifier) < 43 || len(verifier) > 128 {
		return false
	}
	sum := sha256.Sum256([]byte(verifier))
	return subtle.ConstantTimeCompare([]byte(base64.RawURLEncoding.EncodeToString(sum[:])), []byte(challenge)) == 1
}

func (h *hub) oauthToken(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if err := r.ParseForm(); err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_request", "bad form")
		return
	}
	ctx := r.Context()
	clientID := r.PostFormValue("client_id")
	if !h.resourceOK(r.PostFormValue("resource")) {
		oauthError(w, http.StatusBadRequest, "invalid_target", "this hub only issues tokens for its own /mcp")
		return
	}
	var tokens tenant.OAuthTokens
	var err error
	switch r.PostFormValue("grant_type") {
	case "authorization_code":
		c, cerr := h.reg.TakeOAuthCode(ctx, r.PostFormValue("code"))
		if cerr != nil || c.ClientID != clientID || c.RedirectURI != r.PostFormValue("redirect_uri") || !pkceOK(r.PostFormValue("code_verifier"), c.Challenge) {
			if cerr != nil && !errors.Is(cerr, db.ErrNotFound) {
				slog.Error("oauth code", "err", cerr)
			}
			oauthError(w, http.StatusBadRequest, "invalid_grant", "bad or spent code")
			return
		}
		tokens, err = h.reg.IssueOAuthTokens(ctx, clientID, c.Identity)
	case "refresh_token":
		tokens, err = h.reg.RefreshOAuthTokens(ctx, clientID, r.PostFormValue("refresh_token"))
		if errors.Is(err, db.ErrNotFound) {
			oauthError(w, http.StatusBadRequest, "invalid_grant", "bad or spent refresh token")
			return
		}
	default:
		oauthError(w, http.StatusBadRequest, "unsupported_grant_type", "want authorization_code or refresh_token")
		return
	}
	if err != nil {
		slog.Error("oauth tokens", "err", err)
		oauthError(w, http.StatusInternalServerError, "server_error", "couldn't issue tokens")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{
		"access_token": tokens.Access, "token_type": "Bearer", "expires_in": int(tenant.OAuthAccessTTL.Seconds()),
		"refresh_token": tokens.Refresh, "scope": "mcp",
	})
}
