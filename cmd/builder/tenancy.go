package main

import (
	"context"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/nkirsch/auto-builder/internal/auth"
	"github.com/nkirsch/auto-builder/internal/board"
	"github.com/nkirsch/auto-builder/internal/db"
	"github.com/nkirsch/auto-builder/internal/store"
	"github.com/nkirsch/auto-builder/internal/tenant"
	"github.com/nkirsch/auto-builder/web"
)

// runTenancy serves multi-tenant mode (TENANCY.md): the hub at /, each
// tenant's board at /<slug>/.
func runTenancy(ctx context.Context, cfg config) error {
	reg, err := tenant.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer reg.Close()
	var storage store.Storage = store.Dir(cfg.DataDir)
	if cfg.Bucket != "" {
		if storage, err = store.NewGCS(ctx, cfg.Bucket); err != nil {
			return err
		}
	}
	h, err := newHub(ctx, cfg, reg, storage)
	if err != nil {
		return err
	}
	srv := &http.Server{Addr: ":" + cfg.Port, Handler: logRequests(h.routes()), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.Shutdown(shutdown)
	}()
	slog.Info("listening", "port", cfg.Port, "sha", cfg.GitSHA, "mode", "tenancy")
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// hub serves the tenant list, app creation and platform admin, and mounts
// each tenant's board under its slug.
type hub struct {
	cfg     config
	reg     *tenant.Registry
	auth    *auth.Auth
	storage store.Storage
	tmpl    map[string]*template.Template
	host    string // BASE_URL's host; apps live on <slug>.<host>

	mu     sync.Mutex
	boards map[string]http.Handler
}

func newHub(ctx context.Context, cfg config, reg *tenant.Registry, storage store.Storage) (*hub, error) {
	u, err := url.Parse(cfg.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("BASE_URL: %w", err)
	}
	if strings.TrimSuffix(u.Path, "/") != "" {
		return nil, errors.New("TENANCY=on needs a BASE_URL without a path")
	}
	// ADMIN_EMAIL pairs with ADMIN_LOGIN by position.
	emails := strings.Split(cfg.AdminEmail, ",")
	for i, login := range strings.Split(cfg.AdminLogin, ",") {
		if login = strings.TrimSpace(login); login == "" {
			continue
		}
		email := ""
		if i < len(emails) {
			email = emails[i]
		}
		if err := reg.AddPlatformAdmin(ctx, login, email); err != nil {
			return nil, fmt.Errorf("seed platform admin %s: %w", login, err)
		}
	}
	a, err := auth.New(auth.Config{
		BaseURL: cfg.BaseURL, ClientID: cfg.ClientID, ClientSecret: cfg.ClientSecret,
		GoogleClientID: cfg.GoogleClientID, GoogleClientSecret: cfg.GoogleClientSecret,
		SessionKey: []byte(cfg.SessionKey), DevLogin: cfg.DevLogin, Multi: true, Users: reg,
	}, nil)
	if err != nil {
		return nil, err
	}
	h := &hub{cfg: cfg, reg: reg, auth: a, storage: storage, host: u.Host, boards: map[string]http.Handler{}}
	return h, h.parse()
}

func (h *hub) parse() error {
	h.tmpl = map[string]*template.Template{}
	for _, p := range []string{"login", "index", "new", "admin"} {
		t, err := template.New("").Funcs(template.FuncMap{
			"short": func(s string) string { return s[:min(7, len(s))] },
			"ago":   func(t time.Time) string { return t.Format("Jan 2, 2006") },
		}).ParseFS(web.FS, "hub/layout.html", "hub/"+p+".html")
		if err != nil {
			return fmt.Errorf("hub template %s: %w", p, err)
		}
		h.tmpl[p] = t
	}
	return nil
}

func (h *hub) routes() http.Handler {
	mux := http.NewServeMux()
	h.auth.Routes(mux)
	mux.HandleFunc("GET /auth/login", h.login)
	mux.Handle("GET /static/", http.FileServerFS(web.FS))
	mux.HandleFunc("GET /favicon.ico", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFileFS(w, r, web.FS, "static/favicon.ico")
	})
	mux.HandleFunc("GET /health", h.health)
	mux.HandleFunc("GET /{$}", h.signedIn(h.index))
	mux.HandleFunc("GET /new", h.platformAdmin(h.newApp))
	mux.HandleFunc("POST /new", h.platformAdmin(h.create))
	mux.HandleFunc("GET /admin", h.platformAdmin(h.admin))
	mux.HandleFunc("POST /admin/platform-admins", h.platformAdmin(h.addAdmin))
	mux.HandleFunc("GET /{slug}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/"+r.PathValue("slug")+"/", http.StatusMovedPermanently)
	})
	mux.HandleFunc("/{slug}/", h.board)
	return mux
}

// board serves a tenant's board, building it on first use.
func (h *hub) board(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	if tenant.ValidSlug(slug) != nil {
		http.NotFound(w, r)
		return
	}
	b, err := h.boardFor(r.Context(), slug)
	if errors.Is(err, db.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		slog.Error("board", "tenant", slug, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	b.ServeHTTP(w, r)
}

func (h *hub) boardFor(ctx context.Context, slug string) (http.Handler, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if b, ok := h.boards[slug]; ok {
		return b, nil
	}
	t, err := h.reg.Tenant(ctx, slug)
	if err != nil {
		return nil, err
	}
	st, err := h.reg.Store(ctx, slug)
	if err != nil {
		return nil, err
	}
	prefix := "/" + slug
	ta := h.auth.ForTenant(st, prefix)
	b := &board.Board{
		Store: st, Storage: store.Prefixed{Storage: h.storage, Prefix: "t/" + slug + "/"},
		Auth: ta, Gate: holdGate{}, BaseURL: h.cfg.BaseURL + prefix,
		BotLogin: h.cfg.BotLogin, GitSHA: h.cfg.GitSHA, Repo: t.Repo,
	}
	if h.cfg.Version != "" {
		b.SetVersion(h.cfg.Version)
	}
	mux := http.NewServeMux()
	ta.TenantRoutes(mux)
	if err := b.Routes(mux); err != nil {
		return nil, err
	}
	handler := http.StripPrefix(prefix, prefixLocation(prefix, mux))
	h.boards[slug] = handler
	return handler, nil
}

// holdGate stands in until runs are wired per tenant (TENANCY.md step 3).
type holdGate struct{}

func (holdGate) Activity() {}
func (holdGate) RunNow(context.Context, string) (int, error) {
	return 0, errors.New("runs aren't wired for multi-tenant boards yet")
}

type hubPage struct {
	Title, Site, Me, Flash, GitSHA string
	Admin                          bool
	Data                           any
}

func (h *hub) render(w http.ResponseWriter, r *http.Request, name, title string, data any) {
	me := h.auth.SignedIn(r)
	_, err := h.reg.PlatformAdmin(r.Context(), me)
	p := hubPage{Title: title, Site: h.host, Me: me, Admin: me != "" && err == nil,
		Flash: r.URL.Query().Get("flash"), GitSHA: h.cfg.GitSHA, Data: data}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.tmpl[name].ExecuteTemplate(w, "layout", p); err != nil {
		slog.Error("render", "page", name, "err", err)
	}
}

func (h *hub) login(w http.ResponseWriter, r *http.Request) {
	next := auth.SafeNext(r.URL.Query().Get("next"))
	if h.auth.SignedIn(r) != "" {
		http.Redirect(w, r, next, http.StatusFound)
		return
	}
	h.render(w, r, "login", "Sign in", map[string]any{"Next": next, "GitHub": h.auth.GitHub(), "Google": h.auth.Google()})
}

func (h *hub) signedIn(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if h.auth.SignedIn(r) == "" {
			http.Redirect(w, r, "/auth/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusFound)
			return
		}
		next(w, r)
	}
}

// platformAdmin admits platform admins; POSTs must come from this site.
func (h *hub) platformAdmin(next http.HandlerFunc) http.HandlerFunc {
	return h.signedIn(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && !h.auth.SameOrigin(r) {
			http.Error(w, "cross-origin request refused", http.StatusForbidden)
			return
		}
		if _, err := h.reg.PlatformAdmin(r.Context(), h.auth.SignedIn(r)); err != nil {
			http.Error(w, "platform admins only", http.StatusForbidden)
			return
		}
		next(w, r)
	})
}

// app is one row on the hub's index.
type app struct {
	tenant.Tenant
	Role            string // "" when the viewer is a platform admin but not a member
	Open            int
	AppURL, AppHost string
}

// index lists the apps the viewer belongs to; platform admins see them all.
func (h *hub) index(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	me := h.auth.SignedIn(r)
	_, err := h.reg.PlatformAdmin(ctx, me)
	all := err == nil
	tenants, err := h.reg.Tenants(ctx)
	if err != nil {
		slog.Error("tenants", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	var apps []app
	for _, t := range tenants {
		st, err := h.reg.Store(ctx, t.Slug)
		if err != nil {
			slog.Error("tenant store", "tenant", t.Slug, "err", err)
			continue
		}
		a := app{Tenant: t, AppHost: t.Slug + "." + h.host}
		a.AppURL = "https://" + a.AppHost
		var m db.Member
		if strings.Contains(me, "@") {
			m, err = st.MemberByEmail(ctx, me)
		} else {
			m, err = st.Member(ctx, me)
		}
		if err == nil {
			a.Role = m.Role
		} else if !all {
			continue
		}
		if items, err := st.Items(ctx, "open"); err == nil {
			a.Open = len(items)
		}
		apps = append(apps, a)
	}
	h.render(w, r, "index", "Apps", map[string]any{"Apps": apps})
}

func (h *hub) newApp(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	h.render(w, r, "new", "New app", map[string]any{
		"Slug": q.Get("slug"), "Name": q.Get("name"), "Repo": q.Get("repo"), "Org": h.cfg.TenantOrg, "Host": h.host})
}

// create registers the app and makes its creator the board's admin.
// Provisioning (TENANCY.md step 4) leaves it at "provisioning" for now.
func (h *hub) create(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	slug := strings.TrimSpace(r.FormValue("slug"))
	name := strings.TrimSpace(r.FormValue("name"))
	repo := strings.TrimSpace(r.FormValue("repo"))
	if repo == "" && h.cfg.TenantOrg != "" {
		repo = h.cfg.TenantOrg + "/" + slug
	}
	me, _ := h.reg.PlatformAdmin(ctx, h.auth.SignedIn(r))
	back := func(msg string) {
		v := url.Values{"slug": {slug}, "name": {name}, "repo": {r.FormValue("repo")}, "flash": {msg}}
		http.Redirect(w, r, "/new?"+v.Encode(), http.StatusSeeOther)
	}
	if err := h.reg.Create(ctx, tenant.Tenant{Slug: slug, Name: name, Repo: repo, CreatedBy: me.Login}); err != nil {
		back(err.Error())
		return
	}
	st, err := h.reg.Store(ctx, slug)
	if err == nil {
		err = st.UpsertMember(ctx, me.Login, "admin", me.Login)
	}
	if err == nil && me.Email != "" {
		err = st.SetMemberEmail(ctx, me.Login, me.Email)
	}
	if err != nil {
		slog.Error("seed tenant admin", "tenant", slug, "err", err)
		back("created, but adding you as its admin failed: " + err.Error())
		return
	}
	http.Redirect(w, r, "/"+slug+"/", http.StatusSeeOther)
}

func (h *hub) admin(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	tenants, err := h.reg.Tenants(ctx)
	if err != nil {
		slog.Error("tenants", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	admins, err := h.reg.PlatformAdmins(ctx)
	if err != nil {
		slog.Error("platform admins", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	h.render(w, r, "admin", "Admin", map[string]any{"Tenants": tenants, "Admins": admins})
}

func (h *hub) addAdmin(w http.ResponseWriter, r *http.Request) {
	login := strings.TrimSpace(r.FormValue("login"))
	if login == "" || strings.Contains(login, "@") {
		http.Redirect(w, r, "/admin?flash="+url.QueryEscape("login is a GitHub login, not an address"), http.StatusSeeOther)
		return
	}
	if err := h.reg.AddPlatformAdmin(r.Context(), login, r.FormValue("email")); err != nil {
		http.Redirect(w, r, "/admin?flash="+url.QueryEscape(err.Error()), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func (h *hub) health(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	status, code := "ok", http.StatusOK
	if err := h.reg.Control.Ping(ctx); err != nil {
		status, code = "db: "+err.Error(), http.StatusServiceUnavailable
	}
	writeJSON(w, code, map[string]string{"status": status, "sha": h.cfg.GitSHA})
}
