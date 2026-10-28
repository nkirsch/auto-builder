package main

import (
	"context"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/nkirsch/auto-builder/internal/auth"
	"github.com/nkirsch/auto-builder/internal/board"
	"github.com/nkirsch/auto-builder/internal/db"
	"github.com/nkirsch/auto-builder/internal/gate"
	"github.com/nkirsch/auto-builder/internal/github"
	"github.com/nkirsch/auto-builder/internal/mcp"
	"github.com/nkirsch/auto-builder/internal/notify"
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
	var launcher, provisioner, upstreamer gate.EnvLauncher = gate.LogLauncher{}, gate.LogLauncher{}, gate.LogLauncher{}
	if cfg.Launcher == "cloudrun" {
		if launcher, err = gate.NewCloudRunJob(ctx, cfg.Project, cfg.Region, cfg.Job); err != nil {
			return err
		}
		if cfg.ProvisionJob != "" {
			if provisioner, err = gate.NewCloudRunJob(ctx, cfg.Project, cfg.Region, cfg.ProvisionJob); err != nil {
				return err
			}
		}
		// A hub that doesn't provision apps can still keep up with upstream.
		if cfg.UpstreamJob != "" {
			if upstreamer, err = gate.NewCloudRunJob(ctx, cfg.Project, cfg.Region, cfg.UpstreamJob); err != nil {
				return err
			}
		}
	}
	app, err := github.NewApp(cfg.AppID, cfg.AppInstallation, cfg.AppKey)
	if err != nil {
		return err
	}
	h, err := newHub(ctx, cfg, reg, storage)
	if err != nil {
		return err
	}
	h.launcher, h.provisioner, h.app = launcher, provisioner, app
	if app != nil && cfg.Upstream != "" {
		// The platform repo keeps up with upstream through the provision image (UPSTREAM_JOB).
		h.upstream = &gate.Upstream{State: reg, GitHub: github.ForApp(app), Repo: cfg.Repo, Upstream: cfg.Upstream,
			Branch: "master", Start: func(ctx context.Context) error {
				exec, err := upstreamer.LaunchEnv(ctx, map[string]string{"ACTION": "upstream"})
				if err == nil {
					slog.Info("upstream merge launched", "execution", exec)
				}
				return err
			}}
	}
	h.notify = notify.NewPushover(cfg.PushoverToken, cfg.PushoverUser)
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
	host    string // APP_DOMAIN, else BASE_URL's host; apps live on <slug>.<host>

	launcher    gate.EnvLauncher         // the shared runner job
	provisioner gate.EnvLauncher         // the provision job
	upstream    *gate.Upstream           // nil unless the platform repo has an upstream
	appBase     func(slug string) string // tests: where apps are served instead of <slug>.<host>
	app         *github.App              // nil when the GitHub App isn't configured
	notify      *notify.Pushover

	mu      sync.Mutex
	tenants map[string]*tenantRT
}

// tenantRT is one tenant's board, gate and runner endpoints, built on first use.
type tenantRT struct {
	slug  string
	board http.Handler
	gate  *gate.Gate
	srv   *server // runner API, against this tenant's store and repo
	mcp   http.Handler
	b     *board.Board
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
		SessionKey: []byte(cfg.SessionKey), DevLogin: cfg.DevLogin, Multi: true, Users: reg, Mailboxes: reg,
	}, nil)
	if err != nil {
		return nil, err
	}
	host := u.Host
	if cfg.AppDomain != "" {
		host = cfg.AppDomain
	}
	h := &hub{cfg: cfg, reg: reg, auth: a, storage: storage, host: host, launcher: gate.LogLauncher{},
		provisioner: gate.LogLauncher{}, tenants: map[string]*tenantRT{}}
	return h, h.parse()
}

func (h *hub) parse() error {
	h.tmpl = map[string]*template.Template{}
	for _, p := range []string{"login", "index", "new", "admin", "sites", "urgent"} {
		t, err := template.New("").Funcs(template.FuncMap{
			"short": func(s string) string { return s[:min(7, len(s))] },
			"ago":   func(t time.Time) string { return t.Format("Jan 2, 2006") },
			"when":  func(t time.Time) string { return t.Local().Format("Jan 2 15:04") },
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
	mux.HandleFunc("GET /new", h.creator(h.newApp))
	mux.HandleFunc("POST /new", h.creator(h.create))
	mux.HandleFunc("POST /admin/creators", h.platformAdmin(h.addCreator))
	mux.HandleFunc("POST /admin/creators/{identity}/delete", h.platformAdmin(h.removeCreator))
	mux.HandleFunc("GET /admin", h.platformAdmin(h.admin))
	mux.HandleFunc("GET /admin/sites", h.platformAdmin(h.sites))
	mux.HandleFunc("POST /admin/platform-admins", h.platformAdmin(h.addAdmin))
	mux.HandleFunc("POST /admin/platform-admins/{login}/delete", h.platformAdmin(h.removeAdmin))
	mux.HandleFunc("POST /admin/tenants/{slug}/provision", h.platformAdmin(h.reprovision))
	mux.HandleFunc("POST /admin/tenants/{slug}/delete", h.platformAdmin(h.deleteApp))
	mux.HandleFunc("POST /admin/upstream", h.platformAdmin(h.setUpstream))
	mux.HandleFunc("GET /admin/urgent", h.platformAdmin(h.urgent))
	mux.HandleFunc("POST /admin/tenants/{slug}/items/{id}/reassign", h.platformAdmin(h.reassign))
	mux.HandleFunc("GET /{slug}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/"+r.PathValue("slug")+"/", http.StatusMovedPermanently)
	})
	mux.HandleFunc("/{slug}/", h.board)

	// The runner's endpoints take its run's RUN_TOKEN, which names the tenant.
	for _, m := range []string{"GET", "POST", "DELETE"} {
		mux.HandleFunc(m+" /mcp", h.runner(func(rt *tenantRT, _ int, w http.ResponseWriter, r *http.Request) {
			rt.mcp.ServeHTTP(w, r)
		}))
	}
	run := func(f func(*server, http.ResponseWriter, *http.Request)) http.HandlerFunc {
		return h.runner(func(rt *tenantRT, id int, w http.ResponseWriter, r *http.Request) {
			if r.PathValue("id") != fmt.Sprint(id) {
				http.Error(w, "token is for another run", http.StatusForbidden)
				return
			}
			f(rt.srv, w, r)
		})
	}
	mux.HandleFunc("POST /api/runner/runs/{id}/claim", run((*server).claim))
	mux.HandleFunc("POST /api/runner/runs/{id}/token", run((*server).token))
	mux.HandleFunc("POST /api/runner/runs/{id}/stage", run((*server).stage))
	mux.HandleFunc("POST /api/runner/runs/{id}/finish", run(func(s *server, w http.ResponseWriter, r *http.Request) {
		s.finish(w, r)
		if err := h.reg.RevokeRunToken(context.WithoutCancel(r.Context()), s.slug, s.runID(r)); err != nil {
			slog.Error("revoke run token", "tenant", s.slug, "err", err)
		}
	}))
	mux.HandleFunc("GET /api/runner/attachments/{id}", h.runner(func(rt *tenantRT, _ int, w http.ResponseWriter, r *http.Request) {
		rt.srv.attachment(w, r)
	}))
	mux.Handle("POST /api/gate/tick", auth.Bearer(h.cfg.TickToken, http.HandlerFunc(h.tick)))
	// Aliases (e.g. hub.<domain>) 308 to BASE_URL's host, where the session
	// cookie and the OAuth callbacks live.
	return redirectHosts(h.cfg.BaseURL, h.cfg.RedirectHosts, mux)
}

// runner authenticates a RUN_TOKEN and hands its tenant and run to f.
func (h *hub) runner(f func(rt *tenantRT, runID int, w http.ResponseWriter, r *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || token == "" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		slug, runID, err := h.reg.RunToken(r.Context(), token)
		if errors.Is(err, db.ErrNotFound) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if err != nil {
			slog.Error("run token", "err", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		rt, err := h.tenantFor(r.Context(), slug)
		if err != nil {
			slog.Error("tenant", "tenant", slug, "err", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		f(rt, runID, w, r)
	}
}

// tick runs every ready tenant's gate duties.
func (h *hub) tick(w http.ResponseWriter, r *http.Request) {
	tenants, err := h.reg.Tenants(r.Context())
	if err != nil {
		slog.Error("tick", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	var errs []error
	for _, t := range tenants {
		if t.Status != "ready" {
			continue
		}
		rt, err := h.tenantFor(r.Context(), t.Slug)
		if err == nil {
			err = rt.gate.Tick(r.Context())
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", t.Slug, err))
		}
	}
	if h.upstream != nil {
		if err := h.upstream.Check(r.Context()); err != nil {
			errs = append(errs, fmt.Errorf("upstream: %w", err))
		}
	}
	if err := errors.Join(errs...); err != nil {
		slog.Error("tick", "err", err)
		writeJSON(w, http.StatusOK, map[string]string{"status": "partial", "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// hold stops launches past the platform's run cap or spend limit.
func (h *hub) hold(ctx context.Context) (string, error) {
	set, err := h.reg.Settings(ctx)
	if err != nil {
		return "", err
	}
	active, err := h.reg.ActiveRuns(ctx)
	if err != nil {
		return "", err
	}
	if active >= set.MaxRuns {
		return fmt.Sprintf("%d runs are active across all apps (the cap is %d)", active, set.MaxRuns), nil
	}
	tenants, err := h.reg.Tenants(ctx)
	if err != nil {
		return "", err
	}
	var spent float64
	for _, t := range tenants {
		st, err := h.reg.Store(ctx, t.Slug)
		if err != nil {
			return "", err
		}
		c, err := st.RunCosts(ctx)
		if err != nil {
			return "", err
		}
		spent += c.Last30Days
	}
	if spent >= set.SpendLimit {
		return fmt.Sprintf("all apps spent $%.2f in 30 days (the limit is $%.2f)", spent, set.SpendLimit), nil
	}
	return "", nil
}

// Cancel revokes the run's token and stops its execution.
func (l tenantLauncher) Cancel(ctx context.Context, runID int, execution string) error {
	if err := l.h.reg.RevokeRunToken(ctx, l.slug, runID); err != nil {
		return err
	}
	if c, ok := l.h.launcher.(gate.Canceler); ok {
		return c.Cancel(ctx, runID, execution)
	}
	return nil
}

// tenantLauncher mints a run token and starts the shared runner job for one tenant.
type tenantLauncher struct {
	h    *hub
	slug string
}

func (l tenantLauncher) Launch(ctx context.Context, runID int) (string, error) {
	token, err := l.h.reg.IssueRunToken(ctx, l.slug, runID)
	if err != nil {
		return "", err
	}
	exec, err := l.h.launcher.LaunchEnv(ctx, map[string]string{
		"RUN_ID": fmt.Sprint(runID), "TENANT": l.slug, "RUN_TOKEN": token})
	if err != nil {
		l.h.reg.RevokeRunToken(context.WithoutCancel(ctx), l.slug, runID)
	}
	return exec, err
}

// board serves a tenant's board, building it on first use.
func (h *hub) board(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	if tenant.ValidSlug(slug) != nil {
		http.NotFound(w, r)
		return
	}
	if t, err := h.reg.Tenant(r.Context(), slug); err == nil && t.Status == "deleted" {
		http.NotFound(w, r)
		return
	}
	rt, err := h.tenantFor(r.Context(), slug)
	if errors.Is(err, db.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		slog.Error("board", "tenant", slug, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	rt.board.ServeHTTP(w, r)
}

func (h *hub) tenantFor(ctx context.Context, slug string) (*tenantRT, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if rt, ok := h.tenants[slug]; ok {
		return rt, nil
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
	base := h.cfg.BaseURL + prefix
	storage := store.Prefixed{Storage: h.storage, Prefix: "t/" + slug + "/"}
	gh := github.New("", "")
	if h.app != nil {
		gh = github.ForApp(h.app)
	}
	g := &gate.Gate{Store: st, Launcher: tenantLauncher{h, slug},
		Idle: time.Duration(h.cfg.IdleHours) * time.Hour, Hold: func(ctx context.Context) (string, error) {
			// No runs until the provisioner has made the repo, nor after delete.
			if t, err := h.reg.Tenant(ctx, slug); err != nil {
				return "", err
			} else if t.Status != "ready" {
				return "the app is " + t.Status + ", not ready", nil
			}
			return h.hold(ctx)
		}}
	ta := h.auth.ForTenant(st, prefix)
	b := &board.Board{
		Store: st, Storage: storage, Auth: ta, Gate: g, BaseURL: base,
		BotLogin: h.cfg.BotLogin, GitSHA: h.cfg.GitSHA, Repo: t.Repo, CodeRepo: h.cfg.Repo, GitHub: gh,
		Name: t.Name, SiteURL: h.appURL(slug), Releases: h.cfg.Releases, Traffic: h.cfg.LogProject != "",
		Platform: hubPlatform{h, slug},
	}
	if ta.CanMail() {
		b.Mailer = ta
	}
	if h.cfg.Version != "" {
		b.SetVersion(h.cfg.Version)
	}
	tools := &mcp.Board{Store: st, BotLogin: h.cfg.BotLogin, BaseURL: base, GitSHA: h.cfg.GitSHA, Reply: b.BotReply}
	if h.app != nil && h.cfg.IssueSync {
		g.Issues = &gate.IssueSync{Store: st, GitHub: gh, Repo: t.Repo, BaseURL: base, Notified: b.Notified}
		b.Filer, b.Welcomer, tools.Filer = g.Issues, g.Issues, g.Issues
		ship, err := h.shipper(ctx, t, st, gh, b.BotReply)
		if err != nil {
			return nil, err
		}
		g.Ship = ship
	}
	mux := http.NewServeMux()
	ta.TenantRoutes(mux)
	if err := b.Routes(mux); err != nil {
		return nil, err
	}
	mux.Handle("GET /logs", ta.RequireAdmin(h.traffic(slug))) // the app's traffic
	scfg := h.cfg
	scfg.Repo, scfg.BaseURL = t.Repo, base
	rt := &tenantRT{
		slug:  slug,
		board: http.StripPrefix(prefix, prefixLocation(prefix, mux)),
		gate:  g,
		srv: &server{cfg: scfg, store: st, storage: storage, gate: g, app: h.app, version: b.Version,
			notify: h.notify, slug: slug},
		mcp: tools.Handler(),
		b:   b,
	}
	h.tenants[slug] = rt
	return rt, nil
}

// shipper marks a tenant's merged items shipped once its app serves them:
// the hub's own SHA for the platform repo, an existing app's /health, else
// the x-<slug> service's revision. nil when there's nothing to read.
func (h *hub) shipper(ctx context.Context, t tenant.Tenant, st *db.Store, gh *github.Client,
	reply func(context.Context, db.Item, string) error) (*gate.Shipper, error) {
	s := &gate.Shipper{Store: st, GitHub: gh, Repo: t.Repo, Reply: reply}
	switch {
	case t.Repo == h.cfg.Repo:
		s.GitSHA = h.cfg.GitSHA
	case t.Existing:
		s.Deployed = gate.HealthSHA{URL: h.appURL(t.Slug)}.SHA
	case h.cfg.Project != "":
		live, err := gate.NewServiceSHA(ctx, h.cfg.Project, h.cfg.Region, "x-"+t.Slug)
		if err != nil {
			return nil, err
		}
		s.Deployed = live.SHA
	default:
		return nil, nil
	}
	return s, nil
}

type hubPage struct {
	Title, Site, Me, Flash, GitSHA string
	CanCreate                      bool   // a creator without an app yet: New app shows
	Feedback                       string // FEEDBACK_URL: where to ask for changes to the platform
	Version, Repo                  string // footer: release name and the code's repo
	MeLogin                        string // the viewer's platform admin login, if any
	Admin                          bool
	Data                           any
}

func (h *hub) render(w http.ResponseWriter, r *http.Request, name, title string, data any) {
	me := h.auth.SignedIn(r)
	admin, err := h.reg.PlatformAdmin(r.Context(), me)
	// Only the index offers New app to creators: canCreate may ask other sites.
	canCreate := false
	if name == "index" && me != "" && err != nil {
		canCreate, _ = h.canCreate(r.Context(), me)
	}
	p := hubPage{Title: title, Site: h.host, Me: me, Admin: me != "" && err == nil, MeLogin: admin.Login, CanCreate: canCreate,
		Flash: r.URL.Query().Get("flash"), GitSHA: h.cfg.GitSHA, Feedback: h.cfg.FeedbackURL, Version: h.cfg.Version, Repo: h.cfg.Repo, Data: data}
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

// creator admits platform admins and creators who may still create their one
// app; POSTs must come from this site.
func (h *hub) creator(next http.HandlerFunc) http.HandlerFunc {
	return h.signedIn(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && !h.auth.SameOrigin(r) {
			http.Error(w, "cross-origin request refused", http.StatusForbidden)
			return
		}
		me := h.auth.SignedIn(r)
		if _, err := h.reg.PlatformAdmin(r.Context(), me); err == nil {
			next(w, r)
			return
		}
		if ok, err := h.canCreate(r.Context(), me); err != nil {
			slog.Error("can create", "identity", me, "err", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		} else if !ok {
			http.Error(w, "you can't create an app here: ask a platform admin to add you as a creator (one app each)", http.StatusForbidden)
			return
		}
		next(w, r)
	})
}

// canCreate reports whether me may create their one app: they're on the
// creators list or a member of a CREATORS_FROM board, and they have no app
// yet (none created, and not the admin of one here or of another SITES
// board: an existing app is their app).
func (h *hub) canCreate(ctx context.Context, me string) (bool, error) {
	if me == "" {
		return false, nil
	}
	if made, err := h.reg.CreatedApp(ctx, me); err != nil || made {
		return false, err
	}
	allowed, err := h.reg.Creator(ctx, me)
	if err != nil {
		return false, err
	}
	tenants, err := h.reg.Tenants(ctx)
	if err != nil {
		return false, err
	}
	for _, t := range tenants {
		if t.Status == "deleted" {
			continue
		}
		st, err := h.reg.Store(ctx, t.Slug)
		if err != nil {
			return false, err
		}
		if roleOf(ctx, st, me) == "admin" {
			return false, nil
		}
	}
	if h.cfg.StatsToken == "" {
		return allowed, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	client := &http.Client{Timeout: 5 * time.Second}
	roles := make([]string, len(h.cfg.Sites))
	var wg sync.WaitGroup
	for i, s := range h.cfg.Sites {
		wg.Add(1)
		go func() {
			defer wg.Done()
			roles[i] = remoteRole(ctx, client, s, h.cfg.StatsToken, me)
		}()
	}
	wg.Wait()
	for i, s := range h.cfg.Sites {
		switch {
		case roles[i] == "":
		case slices.Contains(h.cfg.CreatorsFrom, s.Name):
			allowed = true
		case roles[i] == "admin":
			return false, nil // their own site is their app
		}
	}
	return allowed, nil
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
		if t.Status == "deleted" {
			continue
		}
		st, err := h.reg.Store(ctx, t.Slug)
		if err != nil {
			slog.Error("tenant store", "tenant", t.Slug, "err", err)
			continue
		}
		a := app{Tenant: t, AppURL: h.appURL(t.Slug)}
		a.AppHost = strings.TrimPrefix(a.AppURL, "https://")
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
	_, err := h.reg.PlatformAdmin(r.Context(), h.auth.SignedIn(r))
	h.render(w, r, "new", "New app", map[string]any{
		"Platform": err == nil,
		"Slug":     q.Get("slug"), "Name": q.Get("name"), "Repo": q.Get("repo"), "Owner": q.Get("owner"), "Existing": q.Get("existing") == "on" || h.cfg.ProvisionJob == "",
		"Provisions": h.cfg.ProvisionJob != "",
		"Org":        h.cfg.TenantOrg, "Host": h.host})
}

// create registers the app, makes its creator the board's admin and starts
// provisioning; an existing repo is ready at once and never provisioned.
func (h *hub) create(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	slug := strings.TrimSpace(r.FormValue("slug"))
	name := strings.TrimSpace(r.FormValue("name"))
	repo := strings.TrimSpace(r.FormValue("repo"))
	// Without a provision job, every app is an existing repo.
	existing := r.FormValue("existing") == "on" || h.cfg.ProvisionJob == ""
	identity := h.auth.SignedIn(r)
	me, err := h.reg.PlatformAdmin(ctx, identity)
	createdBy := me.Login
	if err != nil {
		// A creator gets a new repo in the org, provisioned; only platform
		// admins name a repo or bring an existing one.
		if h.cfg.ProvisionJob == "" || h.cfg.TenantOrg == "" {
			http.Error(w, "this hub doesn't provision apps", http.StatusForbidden)
			return
		}
		existing, repo = false, ""
		// created_by is the creator's identity, which CanCreate counts.
		createdBy, me = ownerOf(identity)
	}
	// A platform admin may make the app for someone else: the owner becomes
	// its board's admin and it counts as their one app; the platform admin
	// stays an admin too.
	var owner tenant.Admin
	if o := strings.TrimPrefix(strings.TrimSpace(r.FormValue("owner")), "@"); o != "" && err == nil {
		createdBy, owner = ownerOf(o)
	}
	if repo == "" && h.cfg.TenantOrg != "" && !existing {
		repo = h.cfg.TenantOrg + "/" + slug
	}
	back := func(msg string) {
		v := url.Values{"slug": {slug}, "name": {name}, "repo": {r.FormValue("repo")}, "flash": {msg}}
		if o := r.FormValue("owner"); o != "" {
			v.Set("owner", o)
		}
		if existing {
			v.Set("existing", "on")
		}
		http.Redirect(w, r, "/new?"+v.Encode(), http.StatusSeeOther)
	}
	if err := h.reg.Create(ctx, tenant.Tenant{Slug: slug, Name: name, Repo: repo, CreatedBy: createdBy, Existing: existing}); err != nil {
		back(err.Error())
		return
	}
	st, err := h.reg.Store(ctx, slug)
	for _, a := range []tenant.Admin{me, owner} {
		if err != nil || a.Login == "" {
			continue
		}
		if err = st.UpsertMember(ctx, a.Login, "admin", me.Login); err == nil && a.Email != "" {
			err = st.SetMemberEmail(ctx, a.Login, a.Email)
		}
	}
	if err != nil {
		slog.Error("seed tenant admin", "tenant", slug, "err", err)
		back("created, but adding its admins failed: " + err.Error())
		return
	}
	done := "/" + slug + "/"
	if owner.Login != "" {
		done += "?flash=" + url.QueryEscape(createdBy+" is its admin: they sign in at "+h.cfg.BaseURL+"/ and find it under Apps")
	}
	if existing {
		http.Redirect(w, r, done, http.StatusSeeOther)
		return
	}
	if err := h.provision(ctx, slug, "create"); err != nil {
		back("created, but provisioning didn't start: " + err.Error())
		return
	}
	http.Redirect(w, r, done, http.StatusSeeOther)
}

// ownerOf turns a GitHub login or Google address into an app's created_by
// and its board admin: an address gets a login made from its local part.
func ownerOf(identity string) (string, tenant.Admin) {
	if at := strings.Index(identity, "@"); at > 0 {
		email := strings.ToLower(identity)
		return email, tenant.Admin{Login: memberLogin(identity[:at]), Email: email}
	}
	return identity, tenant.Admin{Login: identity}
}

// provision starts the provision job for one tenant.
func (h *hub) provision(ctx context.Context, slug, action string) error {
	exec, err := h.provisioner.LaunchEnv(ctx, map[string]string{"TENANT": slug, "ACTION": action})
	if err != nil {
		slog.Error("provision", "tenant", slug, "err", err)
		return err
	}
	slog.Info("provisioning", "tenant", slug, "action", action, "execution", exec)
	return nil
}

// deleteApp tears an app down once its slug is typed to confirm: the
// provisioner removes its cloud resources and archives its repo. An existing
// repo's app only leaves the hub; its repo and deployment are untouched.
func (h *hub) deleteApp(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	msg := "Deleting " + slug
	t, err := h.reg.Tenant(r.Context(), slug)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if r.FormValue("confirm") != slug {
		msg = "Type the slug to delete " + slug
	} else if t.Existing {
		msg = "Removed " + slug + " from the hub"
		if err := h.reg.SetStatus(r.Context(), slug, "deleted", ""); err != nil {
			msg = err.Error()
		}
	} else if err := h.reg.SetStatus(r.Context(), slug, "deleting", ""); err != nil {
		msg = err.Error()
	} else if err := h.provision(r.Context(), slug, "delete"); err != nil {
		msg = "Delete didn't start: " + err.Error()
	}
	http.Redirect(w, r, "/admin?flash="+url.QueryEscape(msg), http.StatusSeeOther)
}

// reprovision re-runs the provisioner, filling in whatever is missing.
func (h *hub) reprovision(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	t, err := h.reg.Tenant(r.Context(), slug)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if t.Existing {
		http.Redirect(w, r, "/admin?flash="+url.QueryEscape(slug+" is an existing repo; nothing to provision"), http.StatusSeeOther)
		return
	}
	msg := "Provisioning " + slug
	if err := h.provision(r.Context(), slug, "sync"); err != nil {
		msg = "Provisioning didn't start: " + err.Error()
	}
	http.Redirect(w, r, "/admin?flash="+url.QueryEscape(msg), http.StatusSeeOther)
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
	var up map[string]any
	if h.upstream != nil {
		on, pr, err := h.reg.UpstreamSync(ctx)
		if err != nil {
			slog.Error("upstream sync", "err", err)
		}
		up = map[string]any{"Repo": h.cfg.Upstream, "Platform": h.cfg.Repo, "On": on, "PR": pr}
	}
	creators, err := h.reg.Creators(ctx)
	if err != nil {
		slog.Error("creators", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	h.render(w, r, "admin", "Admin", map[string]any{"Tenants": tenants, "Admins": admins, "Upstream": up, "Creators": creators})
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

// addCreator lets a GitHub login or Google address create one app.
func (h *hub) addCreator(w http.ResponseWriter, r *http.Request) {
	identity := strings.TrimSpace(r.FormValue("identity"))
	msg := "Added " + identity + ": they can create one app from New app"
	if err := h.reg.AddCreator(r.Context(), identity, h.auth.SignedIn(r)); err != nil {
		msg = err.Error()
	}
	http.Redirect(w, r, "/admin?flash="+url.QueryEscape(msg), http.StatusSeeOther)
}

// removeCreator stops identity creating an app; an app they made stays.
func (h *hub) removeCreator(w http.ResponseWriter, r *http.Request) {
	identity := r.PathValue("identity")
	msg := "Removed " + identity + " from creators"
	if err := h.reg.RemoveCreator(r.Context(), identity); err != nil {
		msg = err.Error()
	}
	http.Redirect(w, r, "/admin?flash="+url.QueryEscape(msg), http.StatusSeeOther)
}

// memberLogin turns an address's local part into a board login.
func memberLogin(local string) string {
	var b strings.Builder
	for _, c := range strings.ToLower(local) {
		if c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' {
			b.WriteRune(c)
		}
	}
	if b.Len() == 0 {
		return "owner"
	}
	return b.String()
}

// setUpstream turns keeping the platform repo up with upstream on or off.
func (h *hub) setUpstream(w http.ResponseWriter, r *http.Request) {
	on := r.FormValue("on") == "true"
	msg := "No longer keeping up with " + h.cfg.Upstream
	if err := h.reg.SetUpstreamSync(r.Context(), on); err != nil {
		msg = err.Error()
	} else if on {
		msg = "Keeping up with " + h.cfg.Upstream
	}
	http.Redirect(w, r, "/admin?flash="+url.QueryEscape(msg), http.StatusSeeOther)
}

// removeAdmin drops a platform admin; you can't remove yourself.
func (h *hub) removeAdmin(w http.ResponseWriter, r *http.Request) {
	login := r.PathValue("login")
	msg := "Removed " + login
	if me, _ := h.reg.PlatformAdmin(r.Context(), h.auth.SignedIn(r)); me.Login == login {
		msg = "You can't remove yourself"
	} else if err := h.reg.RemovePlatformAdmin(r.Context(), login); err != nil {
		msg = err.Error()
	}
	http.Redirect(w, r, "/admin?flash="+url.QueryEscape(msg), http.StatusSeeOther)
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
