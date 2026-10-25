// Package auth implements GitHub and Google OAuth sign-in, signed session
// cookies and member checks.
package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"golang.org/x/oauth2"

	"github.com/nkirsch/auto-builder/internal/db"
)

const (
	sessionCookie = "session"
	stateCookie   = "oauth_state"
	sessionTTL    = 30 * 24 * time.Hour
)

var github = oauth2.Endpoint{
	AuthURL:  "https://github.com/login/oauth/authorize",
	TokenURL: "https://github.com/login/oauth/access_token",
}

var google = oauth2.Endpoint{
	AuthURL:   "https://accounts.google.com/o/oauth2/v2/auth",
	TokenURL:  "https://oauth2.googleapis.com/token",
	AuthStyle: oauth2.AuthStyleInParams,
}

// Config configures Auth.
type Config struct {
	BaseURL            string
	ClientID           string // GitHub
	ClientSecret       string
	GoogleClientID     string
	GoogleClientSecret string
	SessionKey         []byte
	DevLogin           string // honoured only when BaseURL is localhost
}

// Auth handles sign-in and resolves the current member.
type Auth struct {
	cfg      Config
	store    *db.Store
	oauth    *oauth2.Config
	google   *oauth2.Config
	secure   bool
	dev      bool
	api      string // GitHub API base, overridable in tests
	userinfo string // Google OpenID userinfo endpoint, overridable in tests
}

func New(cfg Config, store *db.Store) (*Auth, error) {
	u, err := url.Parse(cfg.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("BASE_URL: %w", err)
	}
	local := u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1"
	if len(cfg.SessionKey) < 16 {
		if !local {
			return nil, errors.New("SESSION_KEY must be at least 16 bytes")
		}
		cfg.SessionKey = make([]byte, 32)
		rand.Read(cfg.SessionKey)
	}
	base := strings.TrimSuffix(cfg.BaseURL, "/")
	return &Auth{
		cfg:   cfg,
		store: store,
		oauth: &oauth2.Config{
			ClientID:     cfg.ClientID,
			ClientSecret: cfg.ClientSecret,
			Endpoint:     github,
			RedirectURL:  base + "/auth/callback",
		},
		google: &oauth2.Config{
			ClientID:     cfg.GoogleClientID,
			ClientSecret: cfg.GoogleClientSecret,
			Endpoint:     google,
			RedirectURL:  base + "/auth/google/callback",
			Scopes:       []string{"openid", "email", "profile"},
		},
		secure:   u.Scheme == "https",
		dev:      local && cfg.DevLogin != "",
		api:      "https://api.github.com",
		userinfo: "https://openidconnect.googleapis.com/v1/userinfo",
	}, nil
}

type ctxKey struct{}

// Member returns the signed-in member set by Require.
func Member(ctx context.Context) db.Member {
	m, _ := ctx.Value(ctxKey{}).(db.Member)
	return m
}

// WithMember is for tests and internal callers that act as a member.
func WithMember(ctx context.Context, m db.Member) context.Context {
	return context.WithValue(ctx, ctxKey{}, m)
}

// Require rejects requests without a member session. Unsigned-in GET requests
// are sent to sign in; non-members get a 403 naming their login. POSTs must
// come from our own origin.
func (a *Auth) Require(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead && !a.sameOrigin(r) {
			http.Error(w, "cross-origin request refused", http.StatusForbidden)
			return
		}
		login := a.login(r)
		if login == "" {
			if r.Method == http.MethodGet || r.Method == http.MethodHead {
				http.Redirect(w, r, "/auth/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusFound)
				return
			}
			http.Error(w, "sign in required", http.StatusUnauthorized)
			return
		}
		m, err := a.store.Member(r.Context(), login)
		if errors.Is(err, db.ErrNotFound) {
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprintf(w, "%s is not a member of this board. Ask the admin to add you.\n", login)
			return
		}
		if err != nil {
			slog.Error("member lookup", "err", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		next.ServeHTTP(w, r.WithContext(WithMember(r.Context(), m)))
	})
}

// RequireAdmin wraps Require and rejects non-admins.
func (a *Auth) RequireAdmin(next http.Handler) http.Handler {
	return a.Require(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !Member(r.Context()).IsAdmin() {
			http.Error(w, "admins only", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	}))
}

func (a *Auth) sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		// Browsers send Origin on cross-site POSTs; its absence plus SameSite=Lax is safe.
		return r.Header.Get("Sec-Fetch-Site") != "cross-site"
	}
	return strings.TrimSuffix(origin, "/") == strings.TrimSuffix(a.cfg.BaseURL, "/")
}

// GitHub and Google report which sign-in buttons to offer; GitHub is the
// default when neither is configured.
func (a *Auth) GitHub() bool { return a.dev || a.oauth.ClientID != "" || a.google.ClientID == "" }
func (a *Auth) Google() bool { return a.google.ClientID != "" }

// SignedIn returns the session's login, or "" when signed out. It does not
// check membership.
func (a *Auth) SignedIn(r *http.Request) string { return a.login(r) }

func (a *Auth) login(r *http.Request) string {
	if a.dev {
		return a.cfg.DevLogin
	}
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return ""
	}
	login, ok := a.verify(c.Value, time.Now())
	if !ok {
		return ""
	}
	return login
}

// sign returns base64(login|expiry).hex(hmac).
func (a *Auth) sign(login string, expiry time.Time) string {
	payload := base64.RawURLEncoding.EncodeToString([]byte(login + "|" + strconv.FormatInt(expiry.Unix(), 10)))
	return payload + "." + a.mac(payload)
}

func (a *Auth) mac(payload string) string {
	h := hmac.New(sha256.New, a.cfg.SessionKey)
	h.Write([]byte(payload))
	return hex.EncodeToString(h.Sum(nil))
}

func (a *Auth) verify(value string, now time.Time) (string, bool) {
	payload, sig, ok := strings.Cut(value, ".")
	if !ok || !hmac.Equal([]byte(sig), []byte(a.mac(payload))) {
		return "", false
	}
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return "", false
	}
	login, exp, ok := strings.Cut(string(raw), "|")
	if !ok {
		return "", false
	}
	unix, err := strconv.ParseInt(exp, 10, 64)
	if err != nil || now.Unix() > unix || login == "" {
		return "", false
	}
	return login, true
}

// Routes registers /auth/github, /auth/callback, /auth/google,
// /auth/google/callback and /auth/logout. The /auth/login page itself is
// served by the board.
func (a *Auth) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /auth/github", a.handleGitHub)
	mux.HandleFunc("GET /auth/callback", a.handleCallback)
	mux.HandleFunc("GET /auth/google", a.handleGoogle)
	mux.HandleFunc("GET /auth/google/callback", a.handleGoogleCallback)
	mux.HandleFunc("POST /auth/logout", a.handleLogout)
}

// handleGitHub starts the OAuth redirect.
func (a *Auth) handleGitHub(w http.ResponseWriter, r *http.Request) {
	if a.oauth.ClientID == "" && !a.dev {
		http.Error(w, "GitHub sign-in is not configured (GITHUB_CLIENT_ID)", http.StatusServiceUnavailable)
		return
	}
	a.start(w, r, a.oauth)
}

// handleGoogle starts the Google OAuth redirect.
func (a *Auth) handleGoogle(w http.ResponseWriter, r *http.Request) {
	if a.google.ClientID == "" && !a.dev {
		http.Error(w, "Google sign-in is not configured (GOOGLE_CLIENT_ID)", http.StatusServiceUnavailable)
		return
	}
	a.start(w, r, a.google)
}

// start sets the state cookie and redirects to the provider.
func (a *Auth) start(w http.ResponseWriter, r *http.Request, oc *oauth2.Config) {
	next := safeNext(r.URL.Query().Get("next"))
	if a.dev {
		http.Redirect(w, r, next, http.StatusFound)
		return
	}
	b := make([]byte, 16)
	rand.Read(b)
	state := hex.EncodeToString(b)
	http.SetCookie(w, &http.Cookie{
		Name: stateCookie, Value: state + "|" + next, Path: "/auth", MaxAge: 600,
		HttpOnly: true, Secure: a.secure, SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, oc.AuthCodeURL(state), http.StatusFound)
}

// checkState validates and clears the state cookie, returning the next path.
func (a *Auth) checkState(w http.ResponseWriter, r *http.Request) (string, bool) {
	c, err := r.Cookie(stateCookie)
	if err != nil {
		http.Error(w, "sign-in expired; try again", http.StatusBadRequest)
		return "", false
	}
	state, next, _ := strings.Cut(c.Value, "|")
	if subtle.ConstantTimeCompare([]byte(state), []byte(r.URL.Query().Get("state"))) != 1 {
		http.Error(w, "bad sign-in state; try again", http.StatusBadRequest)
		return "", false
	}
	http.SetCookie(w, &http.Cookie{Name: stateCookie, Path: "/auth", MaxAge: -1})
	return next, true
}

// setSession signs the member in and returns to next.
func (a *Auth) setSession(w http.ResponseWriter, r *http.Request, login, next string) {
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: a.sign(login, time.Now().Add(sessionTTL)), Path: "/",
		MaxAge: int(sessionTTL.Seconds()), HttpOnly: true, Secure: a.secure, SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, safeNext(next), http.StatusFound)
}

func (a *Auth) handleCallback(w http.ResponseWriter, r *http.Request) {
	next, ok := a.checkState(w, r)
	if !ok {
		return
	}
	tok, err := a.oauth.Exchange(r.Context(), r.URL.Query().Get("code"))
	if err != nil {
		slog.Warn("oauth exchange", "err", err)
		http.Error(w, "GitHub sign-in failed", http.StatusBadGateway)
		return
	}
	user, err := a.githubUser(r.Context(), tok.AccessToken)
	if err != nil {
		slog.Warn("github user", "err", err)
		http.Error(w, "GitHub sign-in failed", http.StatusBadGateway)
		return
	}
	// A member with an email may be a made-up handle that a stranger owns on GitHub.
	if m, err := a.store.Member(r.Context(), user.Login); err == nil && m.Email != "" {
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprintf(w, "%s signs in with Google.\n", user.Login)
		return
	}
	if err := a.store.TouchMember(r.Context(), user.Login, user.Name, user.AvatarURL); err != nil {
		slog.Error("touch member", "err", err)
	}
	a.setSession(w, r, user.Login, next)
}

// handleGoogleCallback signs in the member whose email matches the verified
// Google address; the session carries that member's login.
func (a *Auth) handleGoogleCallback(w http.ResponseWriter, r *http.Request) {
	next, ok := a.checkState(w, r)
	if !ok {
		return
	}
	tok, err := a.google.Exchange(r.Context(), r.URL.Query().Get("code"))
	if err != nil {
		slog.Warn("google exchange", "err", err)
		http.Error(w, "Google sign-in failed", http.StatusBadGateway)
		return
	}
	user, err := a.googleUser(r.Context(), tok.AccessToken)
	if err != nil {
		slog.Warn("google user", "err", err)
		http.Error(w, "Google sign-in failed", http.StatusBadGateway)
		return
	}
	m, err := a.store.MemberByEmail(r.Context(), user.Email)
	if errors.Is(err, db.ErrNotFound) {
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprintf(w, "%s is not a member of this board. Ask the admin to add you.\n", user.Email)
		return
	}
	if err != nil {
		slog.Error("member by email", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if err := a.store.TouchMember(r.Context(), m.Login, user.Name, user.Picture); err != nil {
		slog.Error("touch member", "err", err)
	}
	a.setSession(w, r, m.Login, next)
}

type googleUser struct {
	Email         string `json:"email"`
	EmailVerified bool   `json:"email_verified"`
	Name          string `json:"name"`
	Picture       string `json:"picture"`
}

func (a *Auth) googleUser(ctx context.Context, token string) (googleUser, error) {
	var u googleUser
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, a.userinfo, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return u, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return u, fmt.Errorf("GET userinfo: %s", resp.Status)
	}
	if err := json.NewDecoder(resp.Body).Decode(&u); err != nil {
		return u, err
	}
	if u.Email == "" || !u.EmailVerified {
		return u, errors.New("GET userinfo: no verified email")
	}
	return u, nil
}

func (a *Auth) handleLogout(w http.ResponseWriter, r *http.Request) {
	if !a.sameOrigin(r) {
		http.Error(w, "cross-origin request refused", http.StatusForbidden)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Path: "/", MaxAge: -1})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

type githubUser struct {
	Login     string `json:"login"`
	Name      string `json:"name"`
	AvatarURL string `json:"avatar_url"`
}

func (a *Auth) githubUser(ctx context.Context, token string) (githubUser, error) {
	var u githubUser
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, a.api+"/user", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return u, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return u, fmt.Errorf("GET /user: %s", resp.Status)
	}
	if err := json.NewDecoder(resp.Body).Decode(&u); err != nil {
		return u, err
	}
	if u.Login == "" {
		return u, errors.New("GET /user: empty login")
	}
	return u, nil
}

// safeNext keeps redirects on this site.
// SafeNext keeps redirects on this site.
func SafeNext(next string) string { return safeNext(next) }

func safeNext(next string) string {
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.HasPrefix(next, "/\\") {
		return "/"
	}
	return next
}

// Bearer returns middleware that requires "Authorization: Bearer <token>".
func Bearer(token string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if token == "" || !ok || subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}
