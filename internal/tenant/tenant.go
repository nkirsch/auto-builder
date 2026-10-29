// Package tenant is multi-tenant mode's registry (TENANCY.md): the control
// schema and one lazily opened pool per tenant schema t_<slug>.
package tenant

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nkirsch/auto-builder/internal/db"
)

//go:embed migrations/*.sql
var migrations embed.FS

// ControlSchema holds the tables shared by all tenants.
const ControlSchema = "control"

var (
	slugRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,19}[a-z0-9]$`)
	repoRE = regexp.MustCompile(`^[A-Za-z0-9-]+/[A-Za-z0-9._-]+$`)
	// reserved are hub paths a slug would shadow, and hub, the hub's own
	// host. www is free: the hub is the apex, so www.<domain> can be an app.
	reserved = []string{"api", "auth", "admin", "mcp", "static", "health", "x", "new", "hub"}
)

// ValidSlug reports why slug can't name a tenant, or nil.
func ValidSlug(slug string) error {
	if !slugRE.MatchString(slug) {
		return fmt.Errorf("slug %q: 2–21 of a-z, 0-9 and -, starting with a letter and not ending with -", slug)
	}
	if slices.Contains(reserved, slug) {
		return fmt.Errorf("slug %q is reserved", slug)
	}
	return nil
}

// Schema is the Postgres schema holding a tenant's board.
func Schema(slug string) string { return "t_" + strings.ReplaceAll(slug, "-", "_") }

// Tenant is one app: a repo, its board and its deployment.
type Tenant struct {
	Slug, Name, Repo, Status, CreatedBy string
	CreatedAt                           time.Time
	ProvisionedAt                       *time.Time
	Error                               string
	// Existing is a repo that deploys itself: never provisioned, ready from the start.
	Existing bool
	// Steps is the last provisioning's progress, in ProvisionSteps order.
	Steps []Step
}

// Step is one provisioning step's state: pending, running, done or failed.
type Step struct {
	Name  string `json:"name"`
	State string `json:"state"`
	Error string `json:"error,omitempty"`
}

// ProvisionSteps are the steps of creating an app, in order; the provision
// job reports each one, and the hub's tick the last.
var ProvisionSteps = []string{
	"Job queued", "Job started", "Repo", "Accounts", "Image repo and service",
	"Domain mapping", "Request log view", "DNS record", "Deploy access", "App code",
	"DNS and certificate",
}

// Live reports whether t's repo and service exist, so its board runs:
// ready, or securing (provisioned, waiting on its domain and certificate).
func (t Tenant) Live() bool { return t.Status == "ready" || t.Status == "securing" }

// Settings are the limits across all tenants.
type Settings struct {
	SpendLimit float64
	MaxRuns    int
}

// Registry holds the control pool and a pool per tenant schema.
type Registry struct {
	url     string
	Control *pgxpool.Pool

	mu     sync.Mutex
	stores map[string]*db.Store
}

// Open connects with search_path=control and applies the control migrations.
func Open(ctx context.Context, url string) (*Registry, error) {
	pool, err := schemaPool(ctx, url, ControlSchema, 4)
	if err != nil {
		return nil, err
	}
	if err := db.MigrateFS(ctx, pool, migrations, "migrations"); err != nil {
		pool.Close()
		return nil, fmt.Errorf("control: %w", err)
	}
	return &Registry{url: url, Control: pool, stores: map[string]*db.Store{}}, nil
}

// Close closes every pool.
func (r *Registry) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, s := range r.stores {
		s.Pool.Close()
	}
	r.Control.Close()
}

// schemaPool opens a pool whose connections use schema, creating it if needed.
func schemaPool(ctx context.Context, url, schema string, maxConns int32) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, err
	}
	ident := pgx.Identifier{schema}.Sanitize()
	cfg.ConnConfig.RuntimeParams["search_path"] = ident
	cfg.MaxConns = maxConns
	cfg.MinConns = 0
	cfg.MaxConnIdleTime = time.Minute
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if _, err := pool.Exec(ctx, "CREATE SCHEMA IF NOT EXISTS "+ident); err != nil {
		pool.Close()
		return nil, fmt.Errorf("schema %s: %w", schema, err)
	}
	return pool, nil
}

// Store returns the board store for a registered tenant, opening its pool
// (at most 2 connections) and migrating its schema on first use.
func (r *Registry) Store(ctx context.Context, slug string) (*db.Store, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s, ok := r.stores[slug]; ok {
		return s, nil
	}
	if _, err := r.Tenant(ctx, slug); err != nil {
		return nil, err
	}
	pool, err := schemaPool(ctx, r.url, Schema(slug), 2)
	if err != nil {
		return nil, err
	}
	if err := db.Migrate(ctx, pool); err != nil {
		pool.Close()
		return nil, fmt.Errorf("tenant %s: %w", slug, err)
	}
	s := &db.Store{Pool: pool}
	r.stores[slug] = s
	return s, nil
}

// Create registers a tenant as provisioning, or an existing one as ready,
// and migrates its schema.
func (r *Registry) Create(ctx context.Context, t Tenant) error {
	if err := ValidSlug(t.Slug); err != nil {
		return err
	}
	if !repoRE.MatchString(t.Repo) {
		return fmt.Errorf("repo %q: want owner/name", t.Repo)
	}
	if strings.TrimSpace(t.Name) == "" {
		t.Name = t.Slug
	}
	if _, err := r.Control.Exec(ctx,
		`INSERT INTO tenants (slug, name, repo, created_by, existing, status, provisioned_at)
		VALUES ($1, $2, $3, $4, $5, CASE WHEN $5 THEN 'ready' ELSE 'provisioning' END, CASE WHEN $5 THEN now() END)`,
		t.Slug, strings.TrimSpace(t.Name), t.Repo, t.CreatedBy, t.Existing); err != nil {
		return err
	}
	_, err := r.Store(ctx, t.Slug)
	return err
}

const tenantCols = `slug, name, repo, status, created_by, created_at, provisioned_at, error, existing, steps`

func scanTenant(row pgx.Row) (Tenant, error) {
	var t Tenant
	err := row.Scan(&t.Slug, &t.Name, &t.Repo, &t.Status, &t.CreatedBy, &t.CreatedAt, &t.ProvisionedAt, &t.Error, &t.Existing, &t.Steps)
	if errors.Is(err, pgx.ErrNoRows) {
		return t, db.ErrNotFound
	}
	return t, err
}

// Tenant returns one tenant, or db.ErrNotFound.
func (r *Registry) Tenant(ctx context.Context, slug string) (Tenant, error) {
	return scanTenant(r.Control.QueryRow(ctx, `SELECT `+tenantCols+` FROM tenants WHERE slug = $1`, slug))
}

// Tenants lists every tenant by slug.
func (r *Registry) Tenants(ctx context.Context) ([]Tenant, error) {
	rows, err := r.Control.Query(ctx, `SELECT `+tenantCols+` FROM tenants ORDER BY slug`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Tenant
	for rows.Next() {
		t, err := scanTenant(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// SetStatus records the provisioner's result; ready stamps provisioned_at.
func (r *Registry) SetStatus(ctx context.Context, slug, status, errText string) error {
	tag, err := r.Control.Exec(ctx, `UPDATE tenants SET status = $2, error = $3,
		provisioned_at = CASE WHEN $2 = 'ready' THEN now() ELSE provisioned_at END
		WHERE slug = $1`, slug, status, errText)
	if err == nil && tag.RowsAffected() == 0 {
		return db.ErrNotFound
	}
	return err
}

// StartSteps resets slug's progress to ProvisionSteps, all pending but the
// first, done: the provision job is queued.
func (r *Registry) StartSteps(ctx context.Context, slug string) error {
	steps := make([]Step, len(ProvisionSteps))
	for i, name := range ProvisionSteps {
		steps[i] = Step{Name: name, State: "pending"}
	}
	steps[0].State = "done"
	tag, err := r.Control.Exec(ctx, `UPDATE tenants SET steps = $2 WHERE slug = $1`, slug, steps)
	if err == nil && tag.RowsAffected() == 0 {
		return db.ErrNotFound
	}
	return err
}

// SetStep records one step's state; a step not in slug's progress is ignored.
func (r *Registry) SetStep(ctx context.Context, slug, name, state, errText string) error {
	_, err := r.Control.Exec(ctx, `UPDATE tenants SET steps = coalesce((
		SELECT jsonb_agg(CASE WHEN e->>'name' = $2 THEN jsonb_strip_nulls(jsonb_build_object(
			'name', $2::text, 'state', $3::text, 'error', nullif($4::text, ''))) ELSE e END ORDER BY i)
		FROM jsonb_array_elements(steps) WITH ORDINALITY AS s(e, i)), '[]')
		WHERE slug = $1`, slug, name, state, errText)
	return err
}

// Admin is a platform admin: they may create tenants and see them all.
type Admin struct{ Login, Email string }

// PlatformAdmin finds the admin whose login or email is identity, or db.ErrNotFound.
func (r *Registry) PlatformAdmin(ctx context.Context, identity string) (Admin, error) {
	var a Admin
	err := r.Control.QueryRow(ctx, `SELECT login, coalesce(email, '') FROM platform_admins
		WHERE login = $1 OR lower(email) = lower($1)`, identity).Scan(&a.Login, &a.Email)
	if errors.Is(err, pgx.ErrNoRows) {
		return a, db.ErrNotFound
	}
	return a, err
}

// PlatformAdmins lists every platform admin by login.
func (r *Registry) PlatformAdmins(ctx context.Context) ([]Admin, error) {
	rows, err := r.Control.Query(ctx, `SELECT login, coalesce(email, '') FROM platform_admins ORDER BY login`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Admin
	for rows.Next() {
		var a Admin
		if err := rows.Scan(&a.Login, &a.Email); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// AddPlatformAdmin makes login a platform admin, setting email when given.
func (r *Registry) AddPlatformAdmin(ctx context.Context, login, email string) error {
	_, err := r.Control.Exec(ctx, `INSERT INTO platform_admins (login, email) VALUES ($1, nullif(lower($2), ''))
		ON CONFLICT (login) DO UPDATE SET email = coalesce(EXCLUDED.email, platform_admins.email)`,
		strings.TrimSpace(login), strings.TrimSpace(email))
	return err
}

// RemovePlatformAdmin drops login from the platform admins.
func (r *Registry) RemovePlatformAdmin(ctx context.Context, login string) error {
	tag, err := r.Control.Exec(ctx, `DELETE FROM platform_admins WHERE login = $1`, login)
	if err == nil && tag.RowsAffected() == 0 {
		return db.ErrNotFound
	}
	return err
}

// normIdentity lower-cases an address; GitHub logins keep their case.
func normIdentity(identity string) string {
	identity = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(identity), "@"))
	if strings.Contains(identity, "@") {
		return strings.ToLower(identity)
	}
	return identity
}

// Creators lists who may create an app of their own.
func (r *Registry) Creators(ctx context.Context) ([]string, error) {
	rows, err := r.Control.Query(ctx, `SELECT identity FROM creators ORDER BY identity`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// AddCreator lets identity (a GitHub login or Google address) create one app.
func (r *Registry) AddCreator(ctx context.Context, identity, by string) error {
	if identity = normIdentity(identity); identity == "" {
		return errors.New("enter a GitHub login or an email")
	}
	_, err := r.Control.Exec(ctx, `INSERT INTO creators (identity, added_by) VALUES ($1, $2)
		ON CONFLICT (identity) DO NOTHING`, identity, by)
	return err
}

// RemoveCreator drops identity from the creators; their app stays.
func (r *Registry) RemoveCreator(ctx context.Context, identity string) error {
	tag, err := r.Control.Exec(ctx, `DELETE FROM creators WHERE identity = $1`, normIdentity(identity))
	if err == nil && tag.RowsAffected() == 0 {
		return db.ErrNotFound
	}
	return err
}

// Creator reports whether identity is on the creators list.
func (r *Registry) Creator(ctx context.Context, identity string) (bool, error) {
	var ok bool
	err := r.Control.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM creators WHERE identity = $1)`,
		normIdentity(identity)).Scan(&ok)
	return ok, err
}

// CreatedApp reports whether identity created an app that isn't deleted.
func (r *Registry) CreatedApp(ctx context.Context, identity string) (bool, error) {
	var ok bool
	err := r.Control.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM tenants WHERE created_by = $1 AND status <> 'deleted')`,
		normIdentity(identity)).Scan(&ok)
	return ok, err
}

// SetUser records a signed-in identity's profile (auth.Users).
func (r *Registry) SetUser(ctx context.Context, identity, name, avatar string) error {
	_, err := r.Control.Exec(ctx, `INSERT INTO users (identity, name, avatar_url) VALUES ($1, $2, $3)
		ON CONFLICT (identity) DO UPDATE SET name = EXCLUDED.name, avatar_url = EXCLUDED.avatar_url, last_seen = now()`,
		identity, name, avatar)
	return err
}

// User returns identity's profile; unknown identities have none.
func (r *Registry) User(ctx context.Context, identity string) (name, avatar string, err error) {
	err = r.Control.QueryRow(ctx, `SELECT name, avatar_url FROM users WHERE identity = $1`, identity).Scan(&name, &avatar)
	if errors.Is(err, pgx.ErrNoRows) {
		err = nil
	}
	return name, avatar, err
}

// SetMailbox records identity's connected Gmail (auth.Mailboxes).
func (r *Registry) SetMailbox(ctx context.Context, identity, address, token string) error {
	_, err := r.Control.Exec(ctx, `INSERT INTO users (identity, gmail_address, gmail_token) VALUES ($1, $2, $3)
		ON CONFLICT (identity) DO UPDATE SET gmail_address = EXCLUDED.gmail_address, gmail_token = EXCLUDED.gmail_token`,
		identity, address, token)
	return err
}

// Mailbox returns identity's connected Gmail; "" when none.
func (r *Registry) Mailbox(ctx context.Context, identity string) (address, token string, err error) {
	err = r.Control.QueryRow(ctx, `SELECT gmail_address, gmail_token FROM users WHERE identity = $1`,
		identity).Scan(&address, &token)
	if errors.Is(err, pgx.ErrNoRows) {
		err = nil
	}
	return address, token, err
}

// Settings returns the limits across all tenants.
func (r *Registry) Settings(ctx context.Context) (Settings, error) {
	var s Settings
	err := r.Control.QueryRow(ctx, `SELECT spend_limit_usd::float8, max_runs FROM settings`).Scan(&s.SpendLimit, &s.MaxRuns)
	return s, err
}

// SetSettings changes the limits across all tenants.
func (r *Registry) SetSettings(ctx context.Context, s Settings) error {
	_, err := r.Control.Exec(ctx, `UPDATE settings SET spend_limit_usd = $1, max_runs = $2`, s.SpendLimit, s.MaxRuns)
	return err
}

// UpstreamSync reports whether the platform repo keeps up with its upstream
// and the upstream PR in flight (0 for none).
func (r *Registry) UpstreamSync(ctx context.Context) (on bool, pr int, err error) {
	err = r.Control.QueryRow(ctx, `SELECT upstream_sync, coalesce(upstream_pr, 0) FROM settings`).Scan(&on, &pr)
	return on, pr, err
}

// SetUpstreamSync turns keeping up with upstream on or off.
func (r *Registry) SetUpstreamSync(ctx context.Context, on bool) error {
	_, err := r.Control.Exec(ctx, `UPDATE settings SET upstream_sync = $1`, on)
	return err
}

// SetUpstreamPR records the upstream PR in flight; 0 clears it.
func (r *Registry) SetUpstreamPR(ctx context.Context, pr int) error {
	_, err := r.Control.Exec(ctx, `UPDATE settings SET upstream_pr = nullif($1, 0)`, pr)
	return err
}

// RecordSignIn, SignIns and PruneSignIns keep every board's sign-in attempts
// in control (db.SignInLog): sessions are hub-wide, membership per board.
func (r *Registry) RecordSignIn(ctx context.Context, s db.SignIn, within time.Duration) error {
	return db.RecordSignIn(ctx, r.Control, s, within)
}

func (r *Registry) SignIns(ctx context.Context, board string, identities []string, only bool, since time.Time) ([]db.SignIn, error) {
	return db.SignIns(ctx, r.Control, board, identities, only, since)
}

func (r *Registry) PruneSignIns(ctx context.Context, before time.Time) error {
	return db.PruneSignIns(ctx, r.Control, before)
}
