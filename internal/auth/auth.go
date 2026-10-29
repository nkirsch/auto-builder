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
	hostCookie    = "__Host-session" // multi-tenant mode over https: host-only, so app subdomains can't set it
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

	// Multi is multi-tenant mode (TENANCY.md): signing in records only who
	// you are, a GitHub login or a Google address, and each board resolves
	// membership through ForTenant.
	Multi bool
	Users Users // multi mode: profiles shared by all boards; optional
	// Mailboxes keeps connected Gmail; the store's members when nil.
	Mailboxes Mailboxes
	// SignIns records sign-in attempts; the store when nil.
	SignIns db.SignInLog
	// BoardOf names the board a multi-mode sign-in is for from its next
	// path, or "".
	BoardOf func(next string) string
	// GitHubLinkOnly uses the GitHub OAuth app only for members to connect
	// their GitHub accounts, not to sign in.
	GitHubLinkOnly bool
	// LinkGitHub records identity's connected GitHub account ("" to
	// disconnect); the store's member by login when nil.
	LinkGitHub func(ctx context.Context, identity, github string) error
}

// Users keeps multi-mode profiles, keyed by identity (login or address).
type Users interface {
	SetUser(ctx context.Context, identity, name, avatar string) error
	User(ctx context.Context, identity string) (name, avatar string, err error)
}

// Auth handles sign-in and resolves the current member.
type Auth struct {
	cfg      Config
	store    *db.Store
	oauth    *oauth2.Config
	google   *oauth2.Config
	gmail    *oauth2.Config // Google with gmail.send, for admins' mail
	secure   bool
	dev      bool
	origin   string // scheme://host of BaseURL
	prefix   string // BaseURL's path, e.g. "/iterate"; "" at the root
	authPath string // where the callbacks live, for the state cookie
	tenant   bool   // a board's view of a multi-mode Auth (ForTenant)
	api      string // GitHub API base, overridable in tests
	userinfo string // Google OpenID userinfo endpoint, overridable in tests
	gmailAPI string // Gmail API base, overridable in tests
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
		gmail: &oauth2.Config{
			ClientID:     cfg.GoogleClientID,
			ClientSecret: cfg.GoogleClientSecret,
			Endpoint:     google,
			RedirectURL:  base + "/auth/gmail/callback",
			Scopes:       []string{"openid", "email", "https://www.googleapis.com/auth/gmail.send"},
		},
		secure:   u.Scheme == "https",
		dev:      local && cfg.DevLogin != "",
		origin:   u.Scheme + "://" + u.Host,
		prefix:   strings.TrimSuffix(u.Path, "/"),
		authPath: strings.TrimSuffix(u.Path, "/") + "/auth",
		api:      "https://api.github.com",
		userinfo: "https://openidconnect.googleapis.com/v1/userinfo",
		gmailAPI: "https://gmail.googleapis.com",
	}, nil
}

// ForTenant is a board's view of a multi-mode Auth: membership comes from
// store, and the board is mounted at prefix. Sign-in still completes at the
// hub's callbacks, with one session for every board.
func (a *Auth) ForTenant(store *db.Store, prefix string) *Auth {
	t := *a
	t.store, t.prefix, t.tenant = store, strings.TrimSuffix(prefix, "/"), true
	return &t
}

// TenantRoutes registers a board's sign-in starts and logout; the callbacks
// stay on the hub's Routes.
func (a *Auth) TenantRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /auth/github", a.handleGitHub)
	mux.HandleFunc("GET /auth/google", a.handleGoogle)
	mux.HandleFunc("GET /auth/gmail", a.handleGmail)
	mux.HandleFunc("GET /auth/github/link", a.handleLink)
	mux.HandleFunc("POST /auth/github/unlink", a.handleUnlink)
	mux.HandleFunc("POST /auth/logout", a.handleLogout)
}

func (a *Auth) cookie() string {
	if a.cfg.Multi && a.secure {
		return hostCookie
	}
	return sessionCookie
}

var errGoogleOnly = errors.New("signs in with Google")

// member resolves the session's identity on this board. In multi mode an
// address is a Google sign-in and anything else a GitHub login.
func (a *Auth) member(ctx context.Context, identity string) (db.Member, error) {
	if a.cfg.Multi && strings.Contains(identity, "@") {
		return a.store.MemberByEmail(ctx, identity)
	}
	m, err := a.store.Member(ctx, identity)
	// A member with an email may be a made-up handle that a stranger owns on GitHub.
	if err == nil && a.cfg.Multi && !m.GitHubSignIn() {
		return m, errGoogleOnly
	}
	return m, err
}

// touch refreshes a multi-mode member's name, avatar and last_seen daily.
func (a *Auth) touch(ctx context.Context, identity string, m db.Member) {
	if a.cfg.Users == nil || (m.LastSeen != nil && time.Since(*m.LastSeen) < 24*time.Hour) {
		return
	}
	name, avatar, err := a.cfg.Users.User(ctx, identity)
	if err != nil {
		slog.Warn("user profile", "err", err)
	}
	if err := a.store.TouchMember(ctx, m.Login, name, avatar); err != nil {
		slog.Error("touch member", "err", err)
	}
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
		m, err := a.member(r.Context(), login)
		a.recordFirstVisit(r, login, err)
		if errors.Is(err, db.ErrNotFound) {
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprintf(w, "%s is not a member of this board. Ask the admin to add you.\n", login)
			return
		}
		if errors.Is(err, errGoogleOnly) {
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprintf(w, "%s signs in with Google.\n", login)
			return
		}
		if err != nil {
			slog.Error("member lookup", "err", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if a.cfg.Multi {
			a.touch(r.Context(), login, m)
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

// SameOrigin reports whether a POST comes from this site.
func (a *Auth) SameOrigin(r *http.Request) bool { return a.sameOrigin(r) }

func (a *Auth) sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		// Browsers send Origin on cross-site POSTs; its absence plus SameSite=Lax is safe.
		return r.Header.Get("Sec-Fetch-Site") != "cross-site"
	}
	return strings.TrimSuffix(origin, "/") == a.origin
}

// GitHub and Google report which sign-in buttons to offer; GitHub is the
// default when neither is configured.
func (a *Auth) GitHub() bool {
	return a.dev || (a.oauth.ClientID != "" && !a.cfg.GitHubLinkOnly) || a.google.ClientID == ""
}

func (a *Auth) Google() bool { return a.google.ClientID != "" }

// CanLinkGitHub reports whether members can connect their GitHub accounts.
func (a *Auth) CanLinkGitHub() bool { return a.oauth.ClientID != "" && !a.dev }

// SignedIn returns the session's login, or "" when signed out. It does not
// check membership.
func (a *Auth) SignedIn(r *http.Request) string { return a.login(r) }

func (a *Auth) login(r *http.Request) string {
	login, _ := a.session(r)
	return login
}

// session returns the session's login and when it was signed; "" when signed out.
func (a *Auth) session(r *http.Request) (string, time.Time) {
	if a.dev {
		return a.cfg.DevLogin, time.Time{}
	}
	c, err := r.Cookie(a.cookie())
	if err != nil {
		return "", time.Time{}
	}
	login, expiry, ok := a.verify(c.Value, time.Now())
	if !ok {
		return "", time.Time{}
	}
	// A multi-mode GitHub session ends once GitHub sign-in is off.
	if a.cfg.Multi && !a.GitHub() && !strings.Contains(login, "@") {
		return "", time.Time{}
	}
	return login, expiry.Add(-sessionTTL)
}

// SessionCookie is a signed-in session for identity, as sign-in sets it.
func (a *Auth) SessionCookie(identity string) *http.Cookie {
	return &http.Cookie{Name: a.cookie(), Value: a.sign(identity, time.Now().Add(sessionTTL)), Path: "/"}
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

func (a *Auth) verify(value string, now time.Time) (string, time.Time, bool) {
	payload, sig, ok := strings.Cut(value, ".")
	if !ok || !hmac.Equal([]byte(sig), []byte(a.mac(payload))) {
		return "", time.Time{}, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return "", time.Time{}, false
	}
	login, exp, ok := strings.Cut(string(raw), "|")
	if !ok {
		return "", time.Time{}, false
	}
	unix, err := strconv.ParseInt(exp, 10, 64)
	if err != nil || now.Unix() > unix || login == "" {
		return "", time.Time{}, false
	}
	return login, time.Unix(unix, 0), true
}

// Routes registers /auth/github, /auth/callback, /auth/google,
// /auth/google/callback, /auth/gmail, /auth/gmail/callback,
// /auth/github/link, /auth/github/unlink and /auth/logout.
// The /auth/login page itself is served by the board.
func (a *Auth) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /auth/github", a.handleGitHub)
	mux.HandleFunc("GET /auth/callback", a.handleCallback)
	mux.HandleFunc("GET /auth/google", a.handleGoogle)
	mux.HandleFunc("GET /auth/google/callback", a.handleGoogleCallback)
	mux.HandleFunc("GET /auth/gmail", a.handleGmail)
	mux.HandleFunc("GET /auth/gmail/callback", a.handleGmailCallback)
	mux.HandleFunc("GET /auth/github/link", a.handleLink)
	mux.HandleFunc("POST /auth/github/unlink", a.handleUnlink)
	mux.HandleFunc("POST /auth/logout", a.handleLogout)
}

// handleGitHub starts the OAuth redirect.
func (a *Auth) handleGitHub(w http.ResponseWriter, r *http.Request) {
	if !a.GitHub() || (a.oauth.ClientID == "" && !a.dev) {
		http.Error(w, "GitHub sign-in is off", http.StatusServiceUnavailable)
		return
	}
	a.start(w, r, a.oauth)
}

// handleLink starts connecting the signed-in member's GitHub account.
func (a *Auth) handleLink(w http.ResponseWriter, r *http.Request) {
	if !a.CanLinkGitHub() {
		http.Error(w, "connecting GitHub isn't configured (GITHUB_CLIENT_ID)", http.StatusServiceUnavailable)
		return
	}
	if a.login(r) == "" {
		http.Redirect(w, r, "/auth/login", http.StatusFound)
		return
	}
	a.startLink(w, r, a.oauth, true)
}

// handleUnlink disconnects the signed-in member's GitHub account.
func (a *Auth) handleUnlink(w http.ResponseWriter, r *http.Request) {
	identity := a.login(r)
	if !a.sameOrigin(r) || identity == "" {
		http.Error(w, "sign in first", http.StatusUnauthorized)
		return
	}
	if err := a.linkGitHub(r.Context(), identity, ""); err != nil && !errors.Is(err, db.ErrNotFound) {
		slog.Error("unlink github", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	next := safeNext(r.FormValue("next"))
	if a.tenant {
		next = a.prefix + next
	}
	http.Redirect(w, r, next, http.StatusSeeOther)
}

func (a *Auth) linkGitHub(ctx context.Context, identity, github string) error {
	if a.cfg.LinkGitHub != nil {
		return a.cfg.LinkGitHub(ctx, identity, github)
	}
	if a.store == nil {
		return db.ErrNotFound
	}
	return a.store.LinkGitHub(ctx, identity, github)
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
func (a *Auth) start(w http.ResponseWriter, r *http.Request, oc *oauth2.Config, opts ...oauth2.AuthCodeOption) {
	a.startLink(w, r, oc, false, opts...)
}

// startLink is start; link marks the callback as connecting an account,
// not signing in.
func (a *Auth) startLink(w http.ResponseWriter, r *http.Request, oc *oauth2.Config, link bool, opts ...oauth2.AuthCodeOption) {
	next := safeNext(r.URL.Query().Get("next"))
	if a.dev {
		http.Redirect(w, r, next, http.StatusFound)
		return
	}
	if a.tenant {
		// The hub's callback redirects outside this board's mount.
		next = a.prefix + next
	}
	b := make([]byte, 16)
	rand.Read(b)
	state := hex.EncodeToString(b)
	if link {
		next = "link|" + next // next starts with "/", so this can't be a path
	}
	http.SetCookie(w, &http.Cookie{
		Name: stateCookie, Value: state + "|" + next, Path: a.authPath, MaxAge: 600,
		HttpOnly: true, Secure: a.secure, SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, oc.AuthCodeURL(state, opts...), http.StatusFound)
}

// checkState validates and clears the state cookie, returning the next path.
func (a *Auth) checkState(w http.ResponseWriter, r *http.Request, provider string) (string, bool) {
	next, _, ok := a.checkLinkState(w, r, provider)
	return next, ok
}

// checkLinkState is checkState, also reporting a link started by startLink.
func (a *Auth) checkLinkState(w http.ResponseWriter, r *http.Request, provider string) (next string, link, ok bool) {
	c, err := r.Cookie(stateCookie)
	if err != nil {
		a.record(r, provider, "", db.SignInStateMissed, "")
		http.Error(w, "sign-in expired; try again", http.StatusBadRequest)
		return "", false, false
	}
	state, next, _ := strings.Cut(c.Value, "|")
	if subtle.ConstantTimeCompare([]byte(state), []byte(r.URL.Query().Get("state"))) != 1 {
		a.record(r, provider, "", db.SignInStateBad, next)
		http.Error(w, "bad sign-in state; try again", http.StatusBadRequest)
		return "", false, false
	}
	http.SetCookie(w, &http.Cookie{Name: stateCookie, Path: a.authPath, MaxAge: -1})
	next, link = strings.CutPrefix(next, "link|")
	return next, link, true
}

// setSession signs the member in and returns to next.
func (a *Auth) setSession(w http.ResponseWriter, r *http.Request, login, next string) {
	http.SetCookie(w, &http.Cookie{
		Name: a.cookie(), Value: a.sign(login, time.Now().Add(sessionTTL)), Path: "/",
		MaxAge: int(sessionTTL.Seconds()), HttpOnly: true, Secure: a.secure, SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, safeNext(next), http.StatusFound)
}

func (a *Auth) handleCallback(w http.ResponseWriter, r *http.Request) {
	next, link, ok := a.checkLinkState(w, r, "github")
	if !ok {
		return
	}
	identity := a.login(r)
	if link && identity == "" {
		http.Error(w, "sign in first", http.StatusUnauthorized)
		return
	}
	if !link && !a.GitHub() {
		http.Error(w, "GitHub sign-in is off", http.StatusForbidden)
		return
	}
	tok, err := a.oauth.Exchange(r.Context(), r.URL.Query().Get("code"))
	if err != nil {
		slog.Warn("oauth exchange", "err", err)
		a.record(r, "github", "", db.SignInExchange, next)
		http.Error(w, "GitHub sign-in failed", http.StatusBadGateway)
		return
	}
	user, err := a.githubUser(r.Context(), tok.AccessToken)
	if err != nil {
		slog.Warn("github user", "err", err)
		a.record(r, "github", "", db.SignInProfile, next)
		http.Error(w, "GitHub sign-in failed", http.StatusBadGateway)
		return
	}
	if link {
		if err := a.linkGitHub(r.Context(), identity, user.Login); err != nil {
			slog.Error("link github", "err", err)
			http.Error(w, "connecting GitHub failed", http.StatusInternalServerError)
			return
		}
		slog.Info("linked github", "github", user.Login)
		http.Redirect(w, r, safeNext(next), http.StatusFound)
		return
	}
	if a.cfg.Multi {
		// The board records the outcome on the first visit (recordFirstVisit).
		a.signIn(w, r, user.Login, user.Name, user.AvatarURL, next)
		return
	}
	// A member with an email may be a made-up handle that a stranger owns on GitHub.
	m, err := a.store.Member(r.Context(), user.Login)
	if err == nil && !m.GitHubSignIn() {
		a.record(r, "github", user.Login, db.SignInGoogleOnly, next)
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprintf(w, "%s signs in with Google.\n", user.Login)
		return
	}
	// A non-member gets a session too, and Require turns them away.
	if errors.Is(err, db.ErrNotFound) {
		a.record(r, "github", user.Login, db.SignInNotMember, next)
	} else {
		a.record(r, "github", user.Login, db.SignInOK, next)
	}
	if err := a.store.TouchMember(r.Context(), user.Login, user.Name, user.AvatarURL); err != nil {
		slog.Error("touch member", "err", err)
	}
	a.setSession(w, r, user.Login, next)
}

// handleGoogleCallback signs in the member whose email matches the verified
// Google address; the session carries that member's login.
func (a *Auth) handleGoogleCallback(w http.ResponseWriter, r *http.Request) {
	next, ok := a.checkState(w, r, "google")
	if !ok {
		return
	}
	tok, err := a.google.Exchange(r.Context(), r.URL.Query().Get("code"))
	if err != nil {
		slog.Warn("google exchange", "err", err)
		a.record(r, "google", "", db.SignInExchange, next)
		http.Error(w, "Google sign-in failed", http.StatusBadGateway)
		return
	}
	user, err := a.googleUser(r.Context(), tok.AccessToken)
	if err != nil {
		slog.Warn("google user", "err", err)
		result := db.SignInProfile
		if errors.Is(err, errUnverified) {
			result = db.SignInUnverified
		}
		a.record(r, "google", strings.ToLower(user.Email), result, next)
		http.Error(w, "Google sign-in failed", http.StatusBadGateway)
		return
	}
	if a.cfg.Multi {
		// The board records the outcome on the first visit (recordFirstVisit).
		a.signIn(w, r, strings.ToLower(user.Email), user.Name, user.Picture, next)
		return
	}
	m, err := a.store.MemberByEmail(r.Context(), user.Email)
	if errors.Is(err, db.ErrNotFound) {
		a.record(r, "google", strings.ToLower(user.Email), db.SignInNotMember, next)
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprintf(w, "%s is not a member of this board. Ask the admin to add you.\n", user.Email)
		return
	}
	if err != nil {
		slog.Error("member by email", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	a.record(r, "google", strings.ToLower(user.Email), db.SignInOK, next)
	if err := a.store.TouchMember(r.Context(), m.Login, user.Name, user.Picture); err != nil {
		slog.Error("touch member", "err", err)
	}
	a.setSession(w, r, m.Login, next)
}

// signIn starts a multi-mode session for identity; boards check membership.
func (a *Auth) signIn(w http.ResponseWriter, r *http.Request, identity, name, avatar, next string) {
	if a.cfg.Users != nil {
		if err := a.cfg.Users.SetUser(r.Context(), identity, name, avatar); err != nil {
			slog.Error("set user", "err", err)
		}
	}
	a.setSession(w, r, identity, next)
}

var errUnverified = errors.New("GET userinfo: no verified email")

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
		return u, errUnverified
	}
	return u, nil
}

func (a *Auth) handleLogout(w http.ResponseWriter, r *http.Request) {
	if !a.sameOrigin(r) {
		http.Error(w, "cross-origin request refused", http.StatusForbidden)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: a.cookie(), Path: "/", MaxAge: -1, Secure: a.secure})
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
