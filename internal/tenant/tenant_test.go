package tenant

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nkirsch/auto-builder/internal/db"
)

func TestValidSlug(t *testing.T) {
	for slug, ok := range map[string]bool{
		"ab": true, "www": true, "my-app": true, "a1": true, "abcdefghijklmnopqrstu": true,
		"a": false, "1a": false, "a-": false, "A": false, "a_b": false,
		"abcdefghijklmnopqrstuv": false, "admin": false, "new": false, "x": false, "hub": false,
	} {
		if err := ValidSlug(slug); (err == nil) != ok {
			t.Errorf("%q: %v", slug, err)
		}
	}
	if Schema("my-app") != "t_my_app" {
		t.Errorf("schema %q", Schema("my-app"))
	}
}

// testRegistry drops the control schema and test tenants' schemas first.
// TEST_DATABASE_URL must point at a disposable database.
func testRegistry(t *testing.T) *Registry {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if _, err := pool.Exec(ctx, `DROP SCHEMA IF EXISTS control, t_alpha, t_beta_two CASCADE`); err != nil {
		t.Fatal(err)
	}
	r, err := Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.Close)
	return r
}

func TestTenantsAreIsolated(t *testing.T) {
	ctx := context.Background()
	r := testRegistry(t)
	for _, tn := range []Tenant{
		{Slug: "alpha", Name: "Alpha", Repo: "extreme-dot-org/alpha", CreatedBy: "nkirsch"},
		{Slug: "beta-two", Repo: "extreme-dot-org/beta-two", CreatedBy: "nkirsch"},
	} {
		if err := r.Create(ctx, tn); err != nil {
			t.Fatal(err)
		}
	}
	a, err := r.Store(ctx, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	b, err := r.Store(ctx, "beta-two")
	if err != nil {
		t.Fatal(err)
	}
	ida, err := a.CreateItem(ctx, "in alpha", "", "alice")
	if err != nil {
		t.Fatal(err)
	}
	idb, err := b.CreateItem(ctx, "in beta", "", "bob")
	if err != nil {
		t.Fatal(err)
	}
	if ida != 1 || idb != 1 {
		t.Errorf("item numbers %d, %d; want 1 in each tenant", ida, idb)
	}
	items, err := b.Items(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Title != "in beta" {
		t.Errorf("beta sees %+v", items)
	}
	if again, _ := r.Store(ctx, "alpha"); again != a {
		t.Error("store not cached")
	}
	if got := a.Pool.Config().MaxConns; got != 2 {
		t.Errorf("tenant MaxConns %d", got)
	}

	// The control schema has its own migrations and none of the board's tables.
	var n int
	if err := r.Control.QueryRow(ctx, `SELECT count(*) FROM information_schema.tables
		WHERE table_schema = 'control' AND table_name = 'items'`).Scan(&n); err != nil || n != 0 {
		t.Errorf("control has items table: %d %v", n, err)
	}
}

func TestRegistry(t *testing.T) {
	ctx := context.Background()
	r := testRegistry(t)
	if _, err := r.Store(ctx, "alpha"); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("unregistered store: %v", err)
	}
	for _, slug := range []string{"admin", "api", "prod", "master", "ns", "mail", "login"} {
		if err := r.Create(ctx, Tenant{Slug: slug, Repo: "o/r"}); err == nil || !strings.Contains(err.Error(), "reserved") {
			t.Errorf("reserved slug %q: %v", slug, err)
		}
	}
	if err := r.Create(ctx, Tenant{Slug: "alpha", Repo: "not a repo"}); err == nil {
		t.Error("bad repo accepted")
	}
	if err := r.Create(ctx, Tenant{Slug: "alpha", Repo: "o/alpha", CreatedBy: "nkirsch"}); err != nil {
		t.Fatal(err)
	}
	if err := r.Create(ctx, Tenant{Slug: "beta-two", Repo: "o/alpha", CreatedBy: "nkirsch"}); err == nil {
		t.Error("second tenant on the same repo accepted")
	}
	tn, err := r.Tenant(ctx, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if tn.Name != "alpha" || tn.Status != "provisioning" || tn.ProvisionedAt != nil || tn.Existing || tn.Live() {
		t.Errorf("new tenant %+v", tn)
	}
	if err := r.Create(ctx, Tenant{Slug: "www", Repo: "o/www", CreatedBy: "nkirsch", Existing: true}); err != nil {
		t.Fatal(err)
	}
	if tn, err := r.Tenant(ctx, "www"); err != nil || tn.Status != "ready" || tn.ProvisionedAt == nil || !tn.Existing {
		t.Errorf("existing tenant %+v, %v", tn, err)
	}
	if err := r.SetStatus(ctx, "alpha", "securing", ""); err != nil {
		t.Fatal(err)
	}
	if tn, _ = r.Tenant(ctx, "alpha"); tn.Status != "securing" || tn.ProvisionedAt != nil || !tn.Live() {
		t.Errorf("securing tenant %+v", tn)
	}
	if err := r.SetStatus(ctx, "alpha", "ready", ""); err != nil {
		t.Fatal(err)
	}
	if tn, _ = r.Tenant(ctx, "alpha"); tn.Status != "ready" || tn.ProvisionedAt == nil || !tn.Live() {
		t.Errorf("ready tenant %+v", tn)
	}
	// Progress starts with the job queued; steps record their state in order.
	if err := r.StartSteps(ctx, "alpha"); err != nil {
		t.Fatal(err)
	}
	r.SetStep(ctx, "alpha", "Repo", "done", "")
	r.SetStep(ctx, "alpha", "Accounts", "failed", "quota")
	r.SetStep(ctx, "alpha", "No such step", "done", "")
	tn, _ = r.Tenant(ctx, "alpha")
	if len(tn.Steps) != len(ProvisionSteps) || tn.Steps[0] != (Step{Name: "Job queued", State: "done"}) ||
		tn.Steps[1].State != "pending" || tn.Steps[2] != (Step{Name: "Repo", State: "done"}) ||
		tn.Steps[3] != (Step{Name: "Accounts", State: "failed", Error: "quota"}) {
		t.Errorf("steps %+v", tn.Steps)
	}
	if err := r.StartSteps(ctx, "nobody"); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("steps of missing tenant: %v", err)
	}
	if err := r.SetStatus(ctx, "nobody", "ready", ""); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("status of missing tenant: %v", err)
	}
	if all, err := r.Tenants(ctx); err != nil || len(all) != 2 {
		t.Errorf("tenants %+v %v", all, err)
	}

	if _, err := r.PlatformAdmin(ctx, "nkirsch"); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("admin before adding: %v", err)
	}
	if err := r.AddPlatformAdmin(ctx, "nkirsch", "Nick@Example.org"); err != nil {
		t.Fatal(err)
	}
	if err := r.AddPlatformAdmin(ctx, "nkirsch", ""); err != nil { // keeps the email
		t.Fatal(err)
	}
	for _, id := range []string{"nkirsch", "nick@example.org", "NICK@example.org"} {
		if a, err := r.PlatformAdmin(ctx, id); err != nil || a.Login != "nkirsch" || a.Email != "nick@example.org" {
			t.Errorf("admin %q: %+v %v", id, a, err)
		}
	}
	if all, _ := r.PlatformAdmins(ctx); len(all) != 1 {
		t.Errorf("admins %+v", all)
	}
	r.AddPlatformAdmin(ctx, "setheck", "")
	if err := r.RemovePlatformAdmin(ctx, "setheck"); err != nil {
		t.Fatal(err)
	}
	if err := r.RemovePlatformAdmin(ctx, "setheck"); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("removing twice: %v", err)
	}

	if name, _, err := r.User(ctx, "alice"); err != nil || name != "" {
		t.Errorf("unknown user %q %v", name, err)
	}
	r.SetUser(ctx, "alice", "Alice", "a.png")
	r.SetUser(ctx, "alice", "Alice B", "b.png")
	if name, avatar, _ := r.User(ctx, "alice"); name != "Alice B" || avatar != "b.png" {
		t.Errorf("user %q %q", name, avatar)
	}
	if s, err := r.Settings(ctx); err != nil || s.SpendLimit != 50 || s.MaxRuns != 3 {
		t.Errorf("settings %+v %v", s, err)
	}

	// Reopening finds the control tables already migrated.
	r2, err := Open(ctx, os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	r2.Close()
}

func TestRunTokens(t *testing.T) {
	ctx := context.Background()
	r := testRegistry(t)
	if err := r.Create(ctx, Tenant{Slug: "alpha", Repo: "o/alpha", CreatedBy: "nkirsch"}); err != nil {
		t.Fatal(err)
	}
	tok, err := r.IssueRunToken(ctx, "alpha", 7)
	if err != nil {
		t.Fatal(err)
	}
	if slug, run, err := r.RunToken(ctx, tok); err != nil || slug != "alpha" || run != 7 {
		t.Fatalf("resolve: %s %d %v", slug, run, err)
	}
	if _, _, err := r.RunToken(ctx, tok+"x"); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("wrong token: %v", err)
	}
	var stored int
	r.Control.QueryRow(ctx, `SELECT count(*) FROM run_tokens WHERE encode(hash, 'hex') = $1`, tok).Scan(&stored)
	if stored != 0 {
		t.Error("token stored in the clear")
	}
	if n, _ := r.ActiveRuns(ctx); n != 1 {
		t.Errorf("active runs %d", n)
	}
	if err := r.RevokeRunToken(ctx, "alpha", 7); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.RunToken(ctx, tok); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("revoked token resolves: %v", err)
	}
	tok, _ = r.IssueRunToken(ctx, "alpha", 8)
	r.Control.Exec(ctx, `UPDATE run_tokens SET expires_at = now() - interval '1 second'`)
	if _, _, err := r.RunToken(ctx, tok); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("expired token resolves: %v", err)
	}
	if n, _ := r.ActiveRuns(ctx); n != 0 {
		t.Errorf("active runs after expiry %d", n)
	}
}

func TestCreators(t *testing.T) {
	ctx := context.Background()
	r := testRegistry(t)
	if err := r.AddCreator(ctx, " @jerry ", "nkirsch"); err != nil {
		t.Fatal(err)
	}
	if err := r.AddCreator(ctx, "Momo@Example.org", "nkirsch"); err != nil {
		t.Fatal(err)
	}
	if err := r.AddCreator(ctx, "", "nkirsch"); err == nil {
		t.Error("empty creator accepted")
	}
	if got, _ := r.Creators(ctx); len(got) != 2 || got[0] != "jerry" || got[1] != "momo@example.org" {
		t.Fatalf("creators %q", got)
	}
	if ok, _ := r.Creator(ctx, "momo@EXAMPLE.org"); !ok {
		t.Error("momo not a creator")
	}
	if ok, _ := r.Creator(ctx, "mallory"); ok {
		t.Error("mallory a creator")
	}
	if ok, _ := r.CreatedApp(ctx, "jerry"); ok {
		t.Error("jerry has an app already")
	}
	r.Create(ctx, Tenant{Slug: "jerrys", Repo: "o/jerrys", CreatedBy: "jerry"})
	if ok, _ := r.CreatedApp(ctx, "jerry"); !ok {
		t.Error("jerry's app not counted")
	}
	r.SetStatus(ctx, "jerrys", "deleted", "")
	if ok, _ := r.CreatedApp(ctx, "jerry"); ok {
		t.Error("deleted app counted")
	}
	if err := r.RemoveCreator(ctx, "jerry"); err != nil {
		t.Fatal(err)
	}
	if err := r.RemoveCreator(ctx, "jerry"); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("removed twice: %v", err)
	}
}
