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
	RunnerToken, TickToken                   string
	Bucket, DataDir                          string
	Launcher, Project, Region, Job           string
	AppID, AppInstallation, AppKey           string
	PushoverToken, PushoverUser              string
	IdleHours                                int
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
		Repo:            env("REPO", "nkirsch/auto-builder"),
		AdminLogin:      env("ADMIN_LOGIN", "nkirsch"),
		BotLogin:        env("BOT_LOGIN", "builder-bot"),
		DevLogin:        env("DEV_LOGIN", ""),
		SessionKey:      env("SESSION_KEY", ""),
		ClientID:        env("GITHUB_CLIENT_ID", ""),
		ClientSecret:    env("GITHUB_CLIENT_SECRET", ""),
		RunnerToken:     env("RUNNER_TOKEN", ""),
		TickToken:       env("TICK_TOKEN", ""),
		Bucket:          env("GCS_BUCKET", ""),
		DataDir:         env("DATA_DIR", "./data"),
		Launcher:        env("LAUNCHER", ""),
		Project:         env("GCP_PROJECT", ""),
		Region:          env("GCP_REGION", "us-central1"),
		Job:             env("RUNNER_JOB", "builder-runner"),
		AppID:           env("GH_APP_ID", ""),
		AppInstallation: env("GH_APP_INSTALLATION_ID", ""),
		AppKey:          env("GH_APP_PRIVATE_KEY", ""),
		PushoverToken:   env("PUSHOVER_TOKEN", ""),
		PushoverUser:    env("PUSHOVER_USER", ""),
		IdleHours:       idle,
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

	pool, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	st := &db.Store{Pool: pool}
	if err := st.UpsertMember(ctx, cfg.AdminLogin, "admin", "bootstrap"); err != nil {
		return fmt.Errorf("seed admin: %w", err)
	}

	var storage store.Storage = store.Dir(cfg.DataDir)
	if cfg.Bucket != "" {
		if storage, err = store.NewGCS(ctx, cfg.Bucket); err != nil {
			return err
		}
	}

	var launcher gate.Launcher = gate.LogLauncher{}
	if cfg.Launcher == "cloudrun" {
		if launcher, err = gate.NewCloudRunJob(ctx, cfg.Project, cfg.Region, cfg.Job); err != nil {
			return err
		}
	}

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
	}, st)
	if err != nil {
		return err
	}

	g := &gate.Gate{Store: st, Launcher: launcher, Idle: time.Duration(cfg.IdleHours) * time.Hour}
	b := &board.Board{
		Store: st, Storage: storage, Auth: a, Gate: g,
		Pushover: notify.NewPushover(cfg.PushoverToken, cfg.PushoverUser),
		BaseURL:  cfg.BaseURL, BotLogin: cfg.BotLogin, GitSHA: cfg.GitSHA, Repo: cfg.Repo,
	}
	g.Ship = &gate.Shipper{Store: st, GitHub: gh, Repo: cfg.Repo, GitSHA: cfg.GitSHA, Reply: b.BotReply}
	if cfg.GitSHA != "dev" {
		go resolveVersion(ctx, gh, cfg.Repo, cfg.GitSHA, b)
	}
	tools := &mcp.Board{Store: st, BotLogin: cfg.BotLogin, BaseURL: cfg.BaseURL, GitSHA: cfg.GitSHA, Reply: b.BotReply}

	s := &server{cfg: cfg, store: st, storage: storage, gate: g}
	mux := http.NewServeMux()
	a.Routes(mux)
	if err := b.Routes(mux); err != nil {
		return err
	}
	// Not /healthz: Cloud Run's front end reserves it.
	mux.HandleFunc("GET /health", s.health)
	mux.Handle("/mcp", auth.Bearer(cfg.RunnerToken, tools.Handler()))
	mux.Handle("POST /api/runner/runs/{id}/claim", auth.Bearer(cfg.RunnerToken, http.HandlerFunc(s.claim)))
	mux.Handle("POST /api/runner/runs/{id}/finish", auth.Bearer(cfg.RunnerToken, http.HandlerFunc(s.finish)))
	mux.Handle("GET /api/runner/attachments/{id}", auth.Bearer(cfg.RunnerToken, http.HandlerFunc(s.attachment)))
	mux.Handle("POST /api/gate/tick", auth.Bearer(cfg.TickToken, http.HandlerFunc(s.tick)))

	srv := &http.Server{Addr: ":" + cfg.Port, Handler: logRequests(mux), ReadHeaderTimeout: 10 * time.Second}
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
// retrying briefly; the page simply omits it until then.
func resolveVersion(ctx context.Context, gh *github.Client, repo, sha string, b *board.Board) {
	for attempt := 1; attempt <= 5; attempt++ {
		c, cancel := context.WithTimeout(ctx, 30*time.Second)
		v, err := gh.Version(c, repo, sha)
		cancel()
		if err == nil {
			b.SetVersion(v)
			slog.Info("version", "version", v, "sha", sha)
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

func (s *server) health(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	status, code := "ok", http.StatusOK
	if err := s.store.Pool.Ping(ctx); err != nil {
		status, code = "db: "+err.Error(), http.StatusServiceUnavailable
	}
	writeJSON(w, code, map[string]string{"status": status, "sha": s.cfg.GitSHA})
}

func (s *server) claim(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		http.Error(w, "bad run id", http.StatusBadRequest)
		return
	}
	if err := s.store.ClaimRun(r.Context(), id); errors.Is(err, db.ErrNotFound) {
		http.Error(w, "run is not awaiting a runner", http.StatusConflict)
		return
	} else if err != nil {
		slog.Error("claim", "run", id, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	slog.Info("run claimed", "run", id)
	writeJSON(w, http.StatusOK, map[string]any{"run_id": id, "repo": s.cfg.Repo, "bot_login": s.cfg.BotLogin})
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
		if r.URL.Path != "/health" {
			slog.Info("http", "method", r.Method, "path", r.URL.Path, "status", sw.code, "ms", time.Since(start).Milliseconds())
		}
	})
}
