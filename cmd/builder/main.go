// Command builder serves the board UI, API, MCP endpoint, gate and ship-checker.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/nkirsch/auto-builder/internal/auth"
	"github.com/nkirsch/auto-builder/internal/board"
	"github.com/nkirsch/auto-builder/internal/db"
	"github.com/nkirsch/auto-builder/internal/gate"
	"github.com/nkirsch/auto-builder/internal/github"
	"github.com/nkirsch/auto-builder/internal/mcp"
	"github.com/nkirsch/auto-builder/internal/notify"
	"github.com/nkirsch/auto-builder/internal/store"
)

const maxLog = 20 << 20

type config struct {
	Port, DatabaseURL, BaseURL, GitSHA, Repo string
	AdminLogin, BotLogin, DevLogin           string
	SessionKey, ClientID, ClientSecret       string
	TickToken                                string
	Bucket, DataDir, SiteDir                 string
	Launcher, Project, Region, Job           string
	AppID, AppInstallation, AppKey           string
	PushoverToken, PushoverUser, Quiet       string
	Releases                                 bool
	Version                                  string // set by a deploy that tags the commit
	IdleHours                                int
	IssueSync                                bool // ISSUE_SYNC=off disables importing GitHub issues

	AdminEmail                         string // Google sign-in addresses, paired with ADMIN_LOGIN
	GoogleClientID, GoogleClientSecret string
	RedirectHosts                      []string // old hosts that 308 to BASE_URL's

	Tenancy      bool   // TENANCY=on: multi-tenant mode (TENANCY.md)
	TenantOrg    string // multi mode: GitHub org holding the apps' repos
	ProvisionJob string // multi mode: Cloud Run Job that provisions an app
	UpstreamJob  string // multi mode: UPSTREAM_JOB, the provision image run only for upstream merges; default ProvisionJob
	AppDomain    string // multi mode: APP_DOMAIN, apps on <slug>.<AppDomain>; default BASE_URL's host
	LogProject   string // multi mode: LOG_PROJECT, holding each app's <slug>-requests log view; default GCP_PROJECT
	Upstream     string // the repo this one is a fork of; "" or Repo itself for none
	FeedbackURL  string // multi mode: FEEDBACK_URL, the hub menu's Feedback link; "off" for none

	StatsToken string       // STATS_TOKEN: bearer for GET /api/stats; the hub sends it to SITES
	AdminToken string       // multi mode: ADMIN_TOKEN, bearer for the tenant API (/api/tenants); unset leaves it off
	Sites      []remoteSite // multi mode: SITES, standalone boards on the hub's Apps page
	Hubs       []remoteSite // multi mode: HUBS, other hubs whose /api/overview /admin/hubs gathers
	// multi mode: CREATORS_FROM, names in SITES whose members may create an app
	CreatorsFrom []string
	// single mode: CREATE_APP_URL, a hub's New app page linked for members
	CreateAppURL string
}

func loadConfig() (config, error) {
	idle, err := strconv.Atoi(env("IDLE_HOURS", "6"))
	if err != nil {
		return config{}, fmt.Errorf("IDLE_HOURS: %w", err)
	}
	c := config{
		Port:            env("PORT", "8080"),
		DatabaseURL:     env("DATABASE_URL", ""),
		BaseURL:         strings.TrimSuffix(env("BASE_URL", "http://localhost:8080"), "/"),
		GitSHA:          env("GIT_SHA", "dev"),
		Version:         env("VERSION", ""),
		Repo:            env("REPO", "nkirsch/auto-builder"),
		AdminLogin:      env("ADMIN_LOGIN", "nkirsch"),
		BotLogin:        env("BOT_LOGIN", "builder-bot"),
		DevLogin:        env("DEV_LOGIN", ""),
		SessionKey:      env("SESSION_KEY", ""),
		ClientID:        env("GITHUB_CLIENT_ID", ""),
		ClientSecret:    env("GITHUB_CLIENT_SECRET", ""),
		TickToken:       env("TICK_TOKEN", ""),
		Bucket:          env("GCS_BUCKET", ""),
		DataDir:         env("DATA_DIR", "./data"),
		SiteDir:         env("SITE_DIR", ""),
		Launcher:        env("LAUNCHER", ""),
		Project:         env("GCP_PROJECT", ""),
		Region:          env("GCP_REGION", "us-central1"),
		Job:             env("RUNNER_JOB", "builder-runner"),
		AppID:           env("GH_APP_ID", ""),
		AppInstallation: env("GH_APP_INSTALLATION_ID", ""),
		AppKey:          env("GH_APP_PRIVATE_KEY", ""),
		PushoverToken:   env("PUSHOVER_TOKEN", ""),
		PushoverUser:    env("PUSHOVER_USER", ""),
		Quiet:           env("PUSHOVER_QUIET", "nkirsch"),
		Releases:        env("RELEASES", "false") == "true",
		IdleHours:       idle,
		IssueSync:       env("ISSUE_SYNC", "on") != "off",
	}
	c.AdminEmail = env("ADMIN_EMAIL", "")
	c.GoogleClientID, c.GoogleClientSecret = env("GOOGLE_CLIENT_ID", ""), env("GOOGLE_CLIENT_SECRET", "")
	// GITHUB_SIGN_IN=off leaves Google as the only sign-in, once Google is configured.
	if env("GITHUB_SIGN_IN", "off") == "off" {
		if c.GoogleClientID != "" {
			c.ClientID, c.ClientSecret = "", ""
		} else {
			slog.Warn("GITHUB_SIGN_IN=off ignored: Google sign-in isn't configured")
		}
	}
	c.Tenancy = env("TENANCY", "off") == "on"
	c.TenantOrg = env("TENANT_ORG", "")
	c.ProvisionJob = env("PROVISION_JOB", "")
	c.UpstreamJob = env("UPSTREAM_JOB", c.ProvisionJob)
	c.AppDomain = env("APP_DOMAIN", "")
	if c.FeedbackURL = env("FEEDBACK_URL", "https://builder.extreme.org/items/new"); c.FeedbackURL == "off" {
		c.FeedbackURL = ""
	}
	if c.Upstream = env("UPSTREAM", ""); c.Upstream == c.Repo {
		c.Upstream = ""
	}
	c.StatsToken = env("STATS_TOKEN", "")
	c.AdminToken = env("ADMIN_TOKEN", "")
	c.LogProject = env("LOG_PROJECT", c.Project)
	if c.Sites, err = parseSites(env("SITES", "")); err != nil {
		return c, fmt.Errorf("SITES: %w", err)
	}
	if c.Hubs, err = parseSites(env("HUBS", "")); err != nil {
		return c, fmt.Errorf("HUBS: %w", err)
	}
	c.CreatorsFrom = strings.FieldsFunc(env("CREATORS_FROM", ""), func(r rune) bool { return r == ',' || r == ' ' })
	c.CreateAppURL = env("CREATE_APP_URL", "")
	for _, h := range strings.Split(env("REDIRECT_HOSTS", ""), ",") {
		if h = strings.TrimSpace(h); h != "" {
			c.RedirectHosts = append(c.RedirectHosts, h)
		}
	}
	if c.DatabaseURL == "" {
		return c, errors.New("DATABASE_URL is required")
	}
	if c.Launcher == "" {
		c.Launcher = "none"
		if c.Project != "" {
			c.Launcher = "cloudrun"
		}
	}
	return c, nil
}

// env reads a variable; "unset" is the placeholder bootstrap gives secrets not yet filled in.
func env(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" && v != "unset" {
		return v
	}
	return def
}

type server struct {
	cfg     config
	store   *db.Store
	storage store.Storage
	gate    *gate.Gate
	app     *github.App   // nil when the GitHub App isn't configured
	version func() string // release name, "" until resolved
	notify  *notify.Pushover
	slug    string // multi-tenant mode: the tenant
}

// runID is the run named in the request path, or 0.
func (s *server) runID(r *http.Request) int {
	id, _ := strconv.Atoi(r.PathValue("id"))
	return id
}

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	if err := run(); err != nil {
		slog.Error("exit", "err", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if cfg.Tenancy {
		return runTenancy(ctx, cfg)
	}

	pool, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	st := &db.Store{Pool: pool}
	// ADMIN_EMAIL pairs with ADMIN_LOGIN by position.
	emails := strings.Split(cfg.AdminEmail, ",")
	for i, login := range strings.Split(cfg.AdminLogin, ",") {
		if login = strings.TrimSpace(login); login == "" {
			continue
		}
		if err := st.UpsertMember(ctx, login, "admin", "bootstrap"); err != nil {
			return fmt.Errorf("seed admin %s: %w", login, err)
		}
		if i < len(emails) && strings.TrimSpace(emails[i]) != "" {
			if err := st.SetMemberEmail(ctx, login, emails[i]); err != nil {
				return fmt.Errorf("seed admin email %s: %w", login, err)
			}
		}
	}

	var storage store.Storage = store.Dir(cfg.DataDir)
	if cfg.Bucket != "" {
		if storage, err = store.NewGCS(ctx, cfg.Bucket); err != nil {
			return err
		}
	}

	var envLauncher gate.EnvLauncher = gate.LogLauncher{}
	if cfg.Launcher == "cloudrun" {
		if envLauncher, err = gate.NewCloudRunJob(ctx, cfg.Project, cfg.Region, cfg.Job); err != nil {
			return err
		}
	}
	launcher := gate.TokenLauncher{Store: st, Env: envLauncher}

	gh := github.New("", "")
	app, err := github.NewApp(cfg.AppID, cfg.AppInstallation, cfg.AppKey)
	if err != nil {
		return err
	}
	if app != nil {
		gh = github.ForApp(app)
	}

	a, err := auth.New(auth.Config{
		BaseURL: cfg.BaseURL, ClientID: cfg.ClientID, ClientSecret: cfg.ClientSecret,
		SessionKey: []byte(cfg.SessionKey), DevLogin: cfg.DevLogin,
		GoogleClientID: cfg.GoogleClientID, GoogleClientSecret: cfg.GoogleClientSecret,
	}, st)
	if err != nil {
		return err
	}

	g := &gate.Gate{Store: st, Launcher: launcher, Idle: time.Duration(cfg.IdleHours) * time.Hour}
	push := notify.NewPushover(cfg.PushoverToken, cfg.PushoverUser)
	b := &board.Board{
		Store: st, Storage: storage, Auth: a, Gate: g,
		Pushover: push, Quiet: cfg.Quiet,
		BaseURL: cfg.BaseURL, BotLogin: cfg.BotLogin, GitSHA: cfg.GitSHA, Repo: cfg.Repo,
		GitHub: gh, Releases: cfg.Releases, CreateAppURL: cfg.CreateAppURL, Traffic: cfg.SiteDir != "",
	}
	if a.CanMail() {
		b.Mailer = a
	}
	g.Ship = &gate.Shipper{Store: st, GitHub: gh, Repo: cfg.Repo, GitSHA: cfg.GitSHA, Reply: b.BotReply}
	if app != nil && cfg.IssueSync {
		g.Issues = &gate.IssueSync{Store: st, GitHub: gh, Repo: cfg.Repo, BaseURL: cfg.BaseURL, Notified: b.Notified}
		b.Filer, b.Welcomer = g.Issues, g.Issues
	}
	if app != nil && cfg.Upstream != "" {
		g.Upstream = &gate.Upstream{State: st, GitHub: gh, Repo: cfg.Repo, Upstream: cfg.Upstream,
			Branch: "master", Start: g.StartUpstream}
		if cfg.Releases {
			g.Upstream.Release = releaser(gh, cfg.Repo)
		}
		b.Upstream = cfg.Upstream
	}
	g.SyncSoon()
	if cfg.Version != "" {
		b.SetVersion(cfg.Version)
	} else if cfg.GitSHA != "dev" {
		go resolveVersion(ctx, gh, cfg.Repo, cfg.GitSHA, app != nil, b)
	}
	tools := &mcp.Board{Store: st, BotLogin: cfg.BotLogin, BaseURL: cfg.BaseURL, GitSHA: cfg.GitSHA, Reply: b.BotReply}
	if g.Issues != nil {
		tools.Filer = g.Issues
	}

	s := &server{cfg: cfg, store: st, storage: storage, gate: g, app: app, version: b.Version, notify: push}
	mux := http.NewServeMux()
	a.Routes(mux)
	if err := b.Routes(mux); err != nil {
		return err
	}
	// Not /healthz: Cloud Run's front end reserves it.
	mux.HandleFunc("GET /health", s.health)
	mux.HandleFunc("GET /version", s.versionJSON)                                          // public: a site footer can show it
	mux.Handle("GET /api/stats", auth.Bearer(cfg.StatsToken, http.HandlerFunc(s.stats)))   // the hub's Apps page
	mux.Handle("GET /api/member", auth.Bearer(cfg.StatsToken, http.HandlerFunc(s.member))) // the hub's creator check
	if cfg.SiteDir != "" {
		mux.Handle("GET /logs", a.RequireAdmin(newTraffic(cfg.BaseURL, cfg.Project, os.Getenv("K_SERVICE")))) // site traffic
	}
	s.runnerRoutes(mux, tools.Handler())
	mux.Handle("POST /api/gate/tick", auth.Bearer(cfg.TickToken, http.HandlerFunc(s.tick)))

	handler, err := site(cfg.BaseURL, cfg.SiteDir, mux, b.Version)
	if err != nil {
		return err
	}
	handler = redirectHosts(cfg.BaseURL, cfg.RedirectHosts, handler)
	srv := &http.Server{Addr: ":" + cfg.Port, Handler: logRequests(handler), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.Shutdown(shutdown)
	}()
	slog.Info("listening", "port", cfg.Port, "sha", cfg.GitSHA, "launcher", cfg.Launcher,
		"storage", fmt.Sprintf("%T", storage), "github_app", app != nil)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// resolveVersion looks up the deployed commit's "vYY.MM.DD-N" name for the nav,
// retrying briefly; the page simply omits it until then. With tag set (the
// client is the App's), it also tags the commit with that name.
func resolveVersion(ctx context.Context, gh *github.Client, repo, sha string, tag bool, b *board.Board) {
	for attempt := 1; attempt <= 5; attempt++ {
		c, cancel := context.WithTimeout(ctx, 30*time.Second)
		v, err := gh.Version(c, repo, sha)
		cancel()
		if err == nil {
			b.SetVersion(v)
			slog.Info("version", "version", v, "sha", sha)
			if tag {
				tagRelease(ctx, gh, repo, v, sha)
			}
			return
		}
		slog.Warn("version", "attempt", attempt, "err", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Duration(attempt) * 10 * time.Second):
		}
	}
}

// tagRelease tags sha as version v, retrying briefly; failures are only logged.
// releaser cuts a release of repo's head, for a repo that deploys on release.
func releaser(gh *github.Client, repo string) func(context.Context) error {
	return func(ctx context.Context) error {
		tag, ahead, err := gh.ReleaseHead(ctx, repo, time.Now())
		if err == nil && tag != "" {
			slog.Info("released", "repo", repo, "tag", tag, "commits", ahead)
		}
		return err
	}
}

func tagRelease(ctx context.Context, gh *github.Client, repo, v, sha string) {
	for attempt := 1; attempt <= 3; attempt++ {
		c, cancel := context.WithTimeout(ctx, 30*time.Second)
		err := gh.Tag(c, repo, v, sha)
		cancel()
		if err == nil {
			slog.Info("tagged", "tag", v, "sha", sha)
			return
		}
		slog.Warn("tag", "tag", v, "attempt", attempt, "err", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Duration(attempt) * 10 * time.Second):
		}
	}
}

func (s *server) health(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	status, code := "ok", http.StatusOK
	if err := s.store.Pool.Ping(ctx); err != nil {
		status, code = "db: "+err.Error(), http.StatusServiceUnavailable
	}
	writeJSON(w, code, map[string]string{"status": status, "sha": s.cfg.GitSHA})
}

// versionJSON reports the release name, "" until resolved.
func (s *server) versionJSON(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "public, max-age=300")
	writeJSON(w, http.StatusOK, map[string]string{"version": s.version()})
}

func (s *server) claim(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		http.Error(w, "bad run id", http.StatusBadRequest)
		return
	}
	if err := s.store.ClaimRun(r.Context(), id, s.cfg.GitSHA, s.version()); errors.Is(err, db.ErrNotFound) {
		http.Error(w, "run is not awaiting a runner", http.StatusConflict)
		return
	} else if err != nil {
		slog.Error("claim", "run", id, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	slog.Info("run claimed", "run", id)
	resp := map[string]any{"run_id": id, "repo": s.cfg.Repo, "bot_login": s.cfg.BotLogin}
	if on, err := s.store.AgentWorkflows(r.Context()); err == nil {
		resp["workflows"] = on
	}
	// An upstream run merges upstream instead of running the agent.
	if run, err := s.store.Run(r.Context(), id); err == nil && run.Trigger == "upstream" && s.cfg.Upstream != "" {
		resp["upstream"] = s.cfg.Upstream
	}
	writeJSON(w, http.StatusOK, resp)
}

// token mints a run's one GitHub installation token, so the App's private
// key stays on the server and away from the agent.
func (s *server) token(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		http.Error(w, "bad run id", http.StatusBadRequest)
		return
	}
	if s.app == nil {
		http.Error(w, "GitHub App not configured", http.StatusServiceUnavailable)
		return
	}
	if err := s.store.TakeRunToken(r.Context(), id); errors.Is(err, db.ErrNotFound) {
		http.Error(w, "run is not running or already has its token", http.StatusConflict)
		return
	} else if err != nil {
		slog.Error("token", "run", id, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	// Only this board's repo; workflows only when an admin allows them.
	workflows, err := s.store.AgentWorkflows(r.Context())
	if err != nil {
		slog.Error("token", "run", id, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	perms := github.AgentPermissions(workflows)
	if run, err := s.store.Run(r.Context(), id); err == nil && run.Trigger == "upstream" && s.cfg.Upstream != "" {
		perms = github.UpstreamPermissions
	}
	token, expires, err := s.app.MintFor(r.Context(), s.cfg.Repo, perms)
	if err != nil {
		slog.Error("token", "run", id, "err", err)
		http.Error(w, "minting token failed", http.StatusBadGateway)
		return
	}
	name, email, err := s.app.BotIdentity(r.Context())
	if err != nil {
		slog.Error("bot identity", "run", id, "err", err)
		http.Error(w, "bot identity lookup failed", http.StatusBadGateway)
		return
	}
	slog.Info("run token", "run", id, "expires", expires)
	writeJSON(w, http.StatusOK, tokenResp{Token: token, ExpiresAt: expires, Name: name, Email: email})
}

type tokenResp struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
	Name      string    `json:"name"`  // commit author, "<slug>[bot]"
	Email     string    `json:"email"` // its noreply address
}

// stage records that the runner reached a stage (agent or report).
func (s *server) stage(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		http.Error(w, "bad run id", http.StatusBadRequest)
		return
	}
	var req struct {
		Stage string `json:"stage"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&req); err != nil {
		http.Error(w, "bad body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.Stage != "agent" && req.Stage != "report" {
		http.Error(w, "stage must be agent or report", http.StatusBadRequest)
		return
	}
	if err := s.store.SetRunStage(r.Context(), id, req.Stage); errors.Is(err, db.ErrNotFound) {
		http.Error(w, "run is not running", http.StatusConflict)
		return
	} else if err != nil {
		slog.Error("stage", "run", id, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

type finishReq struct {
	Status  string   `json:"status"`
	Summary string   `json:"summary"`
	Log     string   `json:"log"`
	CostUSD *float64 `json:"cost_usd"` // omitted when the agent reported none
}

func (s *server) finish(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		http.Error(w, "bad run id", http.StatusBadRequest)
		return
	}
	var req finishReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxLog+1<<20)).Decode(&req); err != nil {
		http.Error(w, "bad body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.Status != "done" && req.Status != "failed" {
		http.Error(w, "status must be done or failed", http.StatusBadRequest)
		return
	}
	if req.CostUSD != nil && (*req.CostUSD < 0 || *req.CostUSD >= 1e6) {
		http.Error(w, "cost_usd out of range", http.StatusBadRequest)
		return
	}
	object := ""
	if req.Log != "" {
		object = fmt.Sprintf("runs/%d.log", id)
		if err := s.storage.Put(r.Context(), object, "text/plain; charset=utf-8", strings.NewReader(req.Log)); err != nil {
			slog.Error("store run log", "run", id, "err", err)
			object = ""
		}
	}
	if err := s.store.FinishRun(r.Context(), id, req.Status, req.Summary, object, req.CostUSD); errors.Is(err, db.ErrNotFound) {
		http.Error(w, "run is not active", http.StatusConflict)
		return
	} else if err != nil {
		slog.Error("finish", "run", id, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	slog.Info("run finished", "run", id, "status", req.Status, "cost_usd", req.CostUSD)
	if req.Status == "failed" && outOfCredits(req.Summary+"\n"+req.Log) {
		s.alertCredits(context.WithoutCancel(r.Context()), time.Now())
	}
	s.gate.SyncSoon()
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *server) attachment(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	a, err := s.store.Attachment(r.Context(), id)
	if errors.Is(err, db.ErrNotFound) {
		http.NotFound(w, r)
		return
	} else if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	rc, err := s.storage.Get(r.Context(), a.Object)
	if err != nil {
		http.Error(w, "attachment unavailable", http.StatusNotFound)
		return
	}
	defer rc.Close()
	w.Header().Set("Content-Type", a.ContentType)
	io.Copy(w, rc)
}

func (s *server) tick(w http.ResponseWriter, r *http.Request) {
	if err := s.store.PruneSignIns(r.Context(), time.Now().Add(-db.SignInKeep)); err != nil {
		slog.Error("prune sign-ins", "err", err)
	}
	if err := s.gate.Tick(r.Context()); err != nil {
		slog.Error("tick", "err", err)
		writeJSON(w, http.StatusOK, map[string]string{"status": "partial", "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

type statusWriter struct {
	http.ResponseWriter
	code int
}

func (w *statusWriter) WriteHeader(code int)        { w.code = code; w.ResponseWriter.WriteHeader(code) }
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, code: http.StatusOK}
		next.ServeHTTP(sw, r)
		if !strings.HasSuffix(r.URL.Path, "/health") {
			slog.Info("http", "method", r.Method, "path", r.URL.Path, "status", sw.code, "ms", time.Since(start).Milliseconds())
		}
	})
}
