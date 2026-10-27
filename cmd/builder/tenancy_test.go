package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nkirsch/auto-builder/internal/store"
	"github.com/nkirsch/auto-builder/internal/tenant"
)

// testHub drops the control and test tenants' schemas first; TEST_DATABASE_URL
// must point at a disposable database.
func testHub(t *testing.T) *hub {
	t.Helper()
	u := os.Getenv("TEST_DATABASE_URL")
	if u == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, u)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if _, err := pool.Exec(ctx, `DROP SCHEMA IF EXISTS control, t_alpha, t_beta CASCADE`); err != nil {
		t.Fatal(err)
	}
	reg, err := tenant.Open(ctx, u)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(reg.Close)
	cfg := config{
		BaseURL: "https://hub.example", SessionKey: "0123456789abcdef", GitSHA: "abc1234",
		ClientID: "gh", GoogleClientID: "g", BotLogin: "builder-bot", TenantOrg: "extreme-dot-org",
		Version: "v26.10.25-9", Repo: "kirsch/extreme",
		AdminLogin: "nkirsch", AdminEmail: "nick@example.org", TickToken: "tick", IdleHours: 6,
		ProvisionJob: "extreme-provision",
	}
	h, err := newHub(ctx, cfg, reg, store.Dir(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// do sends a request as identity ("" for signed out); forms are POSTed.
func do(h *hub, handler http.Handler, identity, method, path string, form url.Values) *httptest.ResponseRecorder {
	var r *http.Request
	if form != nil {
		r = httptest.NewRequest(method, path, strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("Origin", "https://hub.example")
	} else {
		r = httptest.NewRequest(method, path, nil)
	}
	r.Host = "hub.example"
	if identity != "" {
		r.AddCookie(h.auth.SessionCookie(identity))
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	return w
}

func TestHubTenants(t *testing.T) {
	ctx := context.Background()
	h := testHub(t)
	srv := h.routes()
	check := func(w *httptest.ResponseRecorder, code int, loc, body string) {
		t.Helper()
		if w.Code != code || w.Header().Get("Location") != loc || !strings.Contains(w.Body.String(), body) {
			t.Fatalf("%d loc=%q body=%.200q; want %d %q %q", w.Code, w.Header().Get("Location"), w.Body.String(), code, loc, body)
		}
	}

	check(do(h, srv, "", "GET", "/", nil), 302, "/auth/login?next=%2F", "")
	check(do(h, srv, "", "GET", "/auth/login", nil), 200, "", `href="https://github.com/kirsch/extreme/tree/v26.10.25-9">v26.10.25-9</a>`)
	check(do(h, srv, "", "GET", "/auth/login?next=/alpha/", nil), 200, "", `href="/auth/google?next=%2falpha%2f"`)

	// Platform admins (seeded from ADMIN_LOGIN/ADMIN_EMAIL) create apps and become their admin.
	check(do(h, srv, "mallory", "GET", "/new", nil), 403, "", "platform admins only")
	check(do(h, srv, "nick@example.org", "POST", "/new", url.Values{"slug": {"admin"}}), 303,
		"/new?flash=slug+%22admin%22+is+reserved&name=&repo=&slug=admin", "")
	check(do(h, srv, "nick@example.org", "POST", "/new", url.Values{"slug": {"alpha"}, "name": {"Alpha"}}), 303, "/alpha/", "")
	check(do(h, srv, "nkirsch", "POST", "/new", url.Values{"slug": {"beta"}, "repo": {"someone/beta"}}), 303, "/beta/", "")
	if tn, err := h.reg.Tenant(ctx, "alpha"); err != nil || tn.Repo != "extreme-dot-org/alpha" || tn.CreatedBy != "nkirsch" {
		t.Fatalf("alpha %+v %v", tn, err)
	}

	// The creator's member has their email, so a board takes Google sign-in only.
	check(do(h, srv, "nick@example.org", "GET", "/alpha/", nil), 200, "", `href="/alpha/items/new"`)
	check(do(h, srv, "nick@example.org", "GET", "/alpha/", nil), 200, "", `<a href="https://alpha.hub.example">Site</a>`)
	check(do(h, srv, "", "GET", "/alpha/auth/login", nil), 200, "", "<h1>Alpha</h1>")
	check(do(h, srv, "nkirsch", "GET", "/alpha/", nil), 403, "", "signs in with Google")
	check(do(h, srv, "mallory", "GET", "/alpha/", nil), 403, "", "not a member")
	check(do(h, srv, "", "GET", "/alpha/items/1", nil), 302, "/alpha/auth/login?next=%2Fitems%2F1", "")
	check(do(h, srv, "", "GET", "/alpha", nil), 301, "/alpha/", "")
	check(do(h, srv, "", "GET", "/nobody/", nil), 404, "", "")
	check(do(h, srv, "", "GET", "/Bad_Slug/", nil), 404, "", "")

	// Members are per board.
	check(do(h, srv, "nick@example.org", "POST", "/alpha/admin/members", url.Values{"login": {"bob"}, "role": {"member"}}), 303, "/alpha/admin?m=Saved%20bob", "")
	check(do(h, srv, "bob", "POST", "/alpha/items", url.Values{"title": {"Dark mode"}, "body": {"please"}}), 303, "/alpha/items/1", "")
	check(do(h, srv, "bob", "GET", "/alpha/items/1", nil), 200, "", "Dark mode")
	check(do(h, srv, "bob", "GET", "/beta/", nil), 403, "", "not a member")
	beta, _ := h.reg.Store(ctx, "beta")
	if items, _ := beta.Items(ctx); len(items) != 0 {
		t.Errorf("beta has alpha's item: %+v", items)
	}

	// The hub lists a member's apps; platform admins see every app.
	w := do(h, srv, "bob", "GET", "/", nil)
	check(w, 200, "", `href="/alpha/"`)
	if strings.Contains(w.Body.String(), `href="/beta/"`) || strings.Contains(w.Body.String(), "/admin") {
		t.Errorf("bob sees beta or admin: %s", w.Body.String())
	}
	w = do(h, srv, "nkirsch", "GET", "/", nil)
	check(w, 200, "", `href="/beta/"`)
	check(w, 200, "", "alpha.hub.example")
	check(do(h, srv, "nkirsch", "GET", "/admin", nil), 200, "", "someone/beta")
	check(do(h, srv, "nkirsch", "POST", "/admin/platform-admins", url.Values{"login": {"katrina"}}), 303, "/admin", "")
	if _, err := h.reg.PlatformAdmin(ctx, "katrina"); err != nil {
		t.Errorf("katrina not a platform admin: %v", err)
	}
	check(do(h, srv, "nkirsch", "GET", "/admin", nil), 200, "", `action="/admin/platform-admins/katrina/delete"`)
	check(do(h, srv, "nkirsch", "GET", "/admin", nil), 200, "", ">Provision</button>")
	h.reg.SetStatus(ctx, "beta", "ready", "")
	check(do(h, srv, "nkirsch", "GET", "/admin", nil), 200, "", ">Re-provision</button>")
	check(do(h, srv, "nkirsch", "POST", "/admin/platform-admins/nkirsch/delete", url.Values{}), 303,
		"/admin?flash=You+can%27t+remove+yourself", "")
	check(do(h, srv, "nkirsch", "POST", "/admin/platform-admins/katrina/delete", url.Values{}), 303, "/admin?flash=Removed+katrina", "")
	if _, err := h.reg.PlatformAdmin(ctx, "katrina"); err == nil {
		t.Error("katrina still a platform admin")
	}

	// Cross-site POSTs are refused.
	r := httptest.NewRequest("POST", "/new", strings.NewReader("slug=gamma"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", "https://evil.example")
	r.AddCookie(h.auth.SessionCookie("nkirsch"))
	w = httptest.NewRecorder()
	srv.ServeHTTP(w, r)
	check(w, 403, "", "cross-origin")
}

// fakeJob records the env each runner launch gets.
type fakeJob struct {
	launches  []map[string]string
	cancelled []string
}

func (f *fakeJob) LaunchEnv(_ context.Context, env map[string]string) (string, error) {
	f.launches = append(f.launches, env)
	return "exec-" + env["RUN_ID"], nil
}

func (f *fakeJob) Cancel(_ context.Context, _ int, execution string) error {
	f.cancelled = append(f.cancelled, execution)
	return nil
}

// runnerDo calls a runner endpoint with a RUN_TOKEN.
func runnerDo(srv http.Handler, token, method, path, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json, text/event-stream")
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, r)
	return w
}

func TestHubRuns(t *testing.T) {
	ctx := context.Background()
	h := testHub(t)
	job, prov := &fakeJob{}, &fakeJob{}
	h.launcher, h.provisioner = job, prov
	srv := h.routes()
	for _, slug := range []string{"alpha", "beta"} {
		if w := do(h, srv, "nkirsch", "POST", "/new", url.Values{"slug": {slug}}); w.Code != 303 {
			t.Fatalf("create %s: %d", slug, w.Code)
		}
		h.reg.SetStatus(ctx, slug, "ready", "")
	}

	// Creating an app provisions it; an admin can provision again.
	if len(prov.launches) != 2 || prov.launches[0]["TENANT"] != "alpha" || prov.launches[0]["ACTION"] != "create" {
		t.Fatalf("provision launches %v", prov.launches)
	}
	if w := do(h, srv, "nkirsch", "POST", "/admin/tenants/alpha/provision", url.Values{}); w.Code != 303 ||
		len(prov.launches) != 3 || prov.launches[2]["ACTION"] != "sync" {
		t.Fatalf("reprovision: %d %v", w.Code, prov.launches)
	}

	// No runs until the app is ready.
	h.reg.SetStatus(ctx, "alpha", "provisioning", "")
	if w := do(h, srv, "nick@example.org", "POST", "/alpha/admin/run", url.Values{}); !strings.Contains(w.Header().Get("Location"), "Held") {
		t.Fatalf("run now while provisioning: %q", w.Header().Get("Location"))
	}
	h.reg.SetStatus(ctx, "alpha", "ready", "")

	// Run now launches the shared job with the tenant and a run token.
	if w := do(h, srv, "nick@example.org", "POST", "/alpha/admin/run", url.Values{}); w.Code != 303 ||
		!strings.Contains(w.Header().Get("Location"), "Started") {
		t.Fatalf("run now: %d %q", w.Code, w.Header().Get("Location"))
	}
	if len(job.launches) != 1 || job.launches[0]["TENANT"] != "alpha" || job.launches[0]["RUN_ID"] != "1" {
		t.Fatalf("launches %v", job.launches)
	}
	tok := job.launches[0]["RUN_TOKEN"]

	claim := runnerDo(srv, tok, "POST", "/api/runner/runs/1/claim", "")
	if claim.Code != 200 || !strings.Contains(claim.Body.String(), `"repo":"extreme-dot-org/alpha"`) {
		t.Fatalf("claim: %d %s", claim.Code, claim.Body.String())
	}
	for _, c := range []struct {
		token, path string
		code        int
	}{
		{"", "/api/runner/runs/1/stage", 401},
		{"bogus", "/api/runner/runs/1/stage", 401},
		{tok, "/api/runner/runs/2/stage", 403}, // another run's id
	} {
		if w := runnerDo(srv, c.token, "POST", c.path, `{"stage":"agent"}`); w.Code != c.code {
			t.Errorf("%q %s: %d, want %d", c.token, c.path, w.Code, c.code)
		}
	}
	if w := runnerDo(srv, tok, "POST", "/api/runner/runs/1/stage", `{"stage":"agent"}`); w.Code != 200 {
		t.Errorf("stage: %d %s", w.Code, w.Body.String())
	}

	// MCP reaches only the token's tenant.
	alpha, _ := h.reg.Store(ctx, "alpha")
	beta, _ := h.reg.Store(ctx, "beta")
	alpha.CreateItem(ctx, "alpha ask", "", "nkirsch")
	beta.CreateItem(ctx, "beta ask", "", "nkirsch")
	alpha.SetApproved(ctx, 1, true, "nkirsch")
	beta.SetApproved(ctx, 1, true, "nkirsch")
	w := runnerDo(srv, tok, "POST", "/mcp", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_items","arguments":{}}}`)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "alpha ask") || strings.Contains(w.Body.String(), "beta ask") {
		t.Fatalf("mcp: %d %s", w.Code, w.Body.String())
	}

	// The platform cap holds other tenants' launches while alpha's run is active.
	h.reg.Control.Exec(ctx, `UPDATE settings SET max_runs = 1`)
	if w := do(h, srv, "nick@example.org", "POST", "/beta/admin/run", url.Values{}); !strings.Contains(w.Header().Get("Location"), "Held") {
		t.Fatalf("beta run now under the cap: %q", w.Header().Get("Location"))
	}
	if len(job.launches) != 1 {
		t.Fatalf("held run launched: %v", job.launches)
	}

	// Finishing revokes the token.
	if w := runnerDo(srv, tok, "POST", "/api/runner/runs/1/finish", `{"status":"done","summary":"ok"}`); w.Code != 200 {
		t.Fatalf("finish: %d %s", w.Code, w.Body.String())
	}
	if w := runnerDo(srv, tok, "POST", "/api/runner/runs/1/stage", `{"stage":"report"}`); w.Code != 401 {
		t.Errorf("revoked token: %d", w.Code)
	}

	// Cancelling a run revokes its token and stops its execution.
	if w := do(h, srv, "nick@example.org", "POST", "/beta/admin/run", url.Values{}); !strings.Contains(w.Header().Get("Location"), "Started") {
		t.Fatalf("beta run now: %q", w.Header().Get("Location"))
	}
	btok := job.launches[len(job.launches)-1]["RUN_TOKEN"]
	if w := do(h, srv, "nick@example.org", "POST", "/beta/runs/1/cancel", url.Values{}); !strings.Contains(w.Header().Get("Location"), "Cancelled") {
		t.Fatalf("cancel: %q", w.Header().Get("Location"))
	}
	if len(job.cancelled) != 1 || job.cancelled[0] != "exec-1" {
		t.Errorf("cancelled %v", job.cancelled)
	}
	if w := runnerDo(srv, btok, "POST", "/api/runner/runs/1/claim", ""); w.Code != 401 {
		t.Errorf("cancelled run's token: %d", w.Code)
	}
	job.launches = job.launches[:len(job.launches)-1]

	// The tick walks ready tenants, launching each one's approved work.
	h.reg.Control.Exec(ctx, `UPDATE settings SET max_runs = 3`)
	tick := httptest.NewRequest("POST", "/api/gate/tick", nil)
	tick.Header.Set("Authorization", "Bearer tick")
	tw := httptest.NewRecorder()
	srv.ServeHTTP(tw, tick)
	if tw.Code != 200 || !strings.Contains(tw.Body.String(), `"ok"`) {
		t.Fatalf("tick: %d %s", tw.Code, tw.Body.String())
	}
	if len(job.launches) != 3 || job.launches[1]["TENANT"] != "alpha" || job.launches[2]["TENANT"] != "beta" {
		t.Fatalf("tick launches %v", job.launches)
	}
	if n, _ := h.reg.ActiveRuns(ctx); n != 2 {
		t.Errorf("active runs %d", n)
	}

	// Deleting needs the slug typed, then starts the provisioner; a deleted app's board is gone.
	if w := do(h, srv, "nkirsch", "POST", "/admin/tenants/beta/delete", url.Values{"confirm": {"nope"}}); !strings.Contains(w.Header().Get("Location"), "Type+the+slug") {
		t.Fatalf("unconfirmed delete: %q", w.Header().Get("Location"))
	}
	if w := do(h, srv, "nkirsch", "POST", "/admin/tenants/beta/delete", url.Values{"confirm": {"beta"}}); w.Code != 303 ||
		prov.launches[len(prov.launches)-1]["ACTION"] != "delete" {
		t.Fatalf("delete: %d %v", w.Code, prov.launches)
	}
	if tn, _ := h.reg.Tenant(ctx, "beta"); tn.Status != "deleting" {
		t.Errorf("beta %s", tn.Status)
	}
	h.reg.SetStatus(ctx, "beta", "deleted", "")
	if w := do(h, srv, "nkirsch", "GET", "/beta/", nil); w.Code != 404 {
		t.Errorf("deleted board: %d", w.Code)
	}
}

func TestHubAliasAndAppDomain(t *testing.T) {
	ctx := context.Background()
	h := testHub(t)
	cfg := h.cfg
	cfg.RedirectHosts, cfg.AppDomain = []string{"alias.example"}, "apps.example"
	h, err := newHub(ctx, cfg, h.reg, h.storage)
	if err != nil {
		t.Fatal(err)
	}
	srv := h.routes()
	get := func(host, path string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", path, nil)
		r.Host = host
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, r)
		return w
	}
	if w := get("alias.example", "/admin?x=1"); w.Code != http.StatusPermanentRedirect ||
		w.Header().Get("Location") != "https://hub.example/admin?x=1" {
		t.Errorf("alias: %d %q", w.Code, w.Header().Get("Location"))
	}
	if w := get("alias.example", "/health"); w.Code == http.StatusPermanentRedirect {
		t.Error("alias redirected /health")
	}
	if w := get("hub.example", "/admin"); w.Code == http.StatusPermanentRedirect {
		t.Error("BASE_URL's host redirected")
	}
	w := do(h, srv, "nkirsch", "GET", "/new", nil)
	if !strings.Contains(w.Body.String(), "&lt;slug&gt;.apps.example") {
		t.Errorf("new page doesn't name APP_DOMAIN: %.300q", w.Body.String())
	}
}

func TestHubFeedbackLink(t *testing.T) {
	h := testHub(t)
	if w := do(h, h.routes(), "nkirsch", "GET", "/", nil); strings.Contains(w.Body.String(), ">Feedback<") {
		t.Error("Feedback link without FEEDBACK_URL")
	}
	h.cfg.FeedbackURL = "https://builder.example/items/new"
	w := do(h, h.routes(), "nkirsch", "GET", "/", nil)
	if !strings.Contains(w.Body.String(), `<a href="https://builder.example/items/new" title="Ask for a change to the platform">Feedback</a>`) {
		t.Errorf("no Feedback link: %.600q", w.Body.String())
	}
}

func TestHubExisting(t *testing.T) {
	ctx := context.Background()
	h := testHub(t)
	prov := &fakeJob{}
	h.provisioner = prov
	srv := h.routes()

	// An existing repo is ready at once and never provisioned.
	for slug, repo := range map[string]string{"alpha": "kirsch/extreme", "beta": "someone/beta"} {
		w := do(h, srv, "nkirsch", "POST", "/new", url.Values{"slug": {slug}, "repo": {repo}, "existing": {"on"}})
		if w.Code != 303 || w.Header().Get("Location") != "/"+slug+"/" {
			t.Fatalf("create %s: %d %q", slug, w.Code, w.Header().Get("Location"))
		}
		if tn, err := h.reg.Tenant(ctx, slug); err != nil || tn.Status != "ready" || !tn.Existing {
			t.Fatalf("%s %+v %v", slug, tn, err)
		}
	}
	if w := do(h, srv, "nkirsch", "POST", "/new", url.Values{"slug": {"gamma"}, "existing": {"on"}}); !strings.Contains(w.Header().Get("Location"), "existing=on") {
		t.Errorf("existing without a repo: %q", w.Header().Get("Location"))
	}
	w := do(h, srv, "nkirsch", "GET", "/admin", nil)
	if strings.Contains(w.Body.String(), "provision</button>") || strings.Contains(w.Body.String(), "Provision</button>") ||
		!strings.Contains(w.Body.String(), `<span class="badge">existing</span>`) {
		t.Errorf("admin page for existing apps: %s", w.Body.String())
	}
	if w := do(h, srv, "nkirsch", "POST", "/admin/tenants/beta/provision", url.Values{}); !strings.Contains(w.Header().Get("Location"), "nothing+to+provision") {
		t.Errorf("reprovision existing: %q", w.Header().Get("Location"))
	}

	// The platform repo ships on the hub's own SHA, another existing app on its /health.
	for slug, platform := range map[string]bool{"alpha": true, "beta": false} {
		tn, _ := h.reg.Tenant(ctx, slug)
		st, _ := h.reg.Store(ctx, slug)
		s, err := h.shipper(ctx, tn, st, nil, nil)
		if err != nil || s == nil || (s.GitSHA == "abc1234") != platform || (s.Deployed != nil) == platform {
			t.Errorf("%s shipper %+v %v", slug, s, err)
		}
	}

	// The footer's version and commit link the hub's repo, not the app's.
	w = do(h, srv, "nick@example.org", "GET", "/beta/", nil)
	if !strings.Contains(w.Body.String(), "github.com/kirsch/extreme/commit/abc1234") || strings.Contains(w.Body.String(), "someone/beta/commit") {
		t.Errorf("tenant footer: %s", w.Body.String())
	}

	// Deleting one only drops it from the hub.
	if w := do(h, srv, "nkirsch", "POST", "/admin/tenants/beta/delete", url.Values{"confirm": {"beta"}}); w.Code != 303 {
		t.Fatalf("delete: %d", w.Code)
	}
	if tn, _ := h.reg.Tenant(ctx, "beta"); tn.Status != "deleted" {
		t.Errorf("beta after delete %+v", tn)
	}
	if len(prov.launches) != 0 {
		t.Errorf("provisioner ran for existing apps: %v", prov.launches)
	}
}

func TestHubWithoutProvisioner(t *testing.T) {
	ctx := context.Background()
	h := testHub(t)
	h.cfg.ProvisionJob = ""
	prov := &fakeJob{}
	h.provisioner = prov
	srv := h.routes()
	if w := do(h, srv, "nkirsch", "GET", "/new", nil); strings.Contains(w.Body.String(), `name="existing"`) ||
		!strings.Contains(w.Body.String(), "doesn't provision") {
		t.Errorf("new page offers provisioning: %s", w.Body.String())
	}
	if w := do(h, srv, "nkirsch", "POST", "/new", url.Values{"slug": {"alpha"}, "repo": {"someone/alpha"}}); w.Code != 303 {
		t.Fatalf("create: %d %q", w.Code, w.Header().Get("Location"))
	}
	if tn, err := h.reg.Tenant(ctx, "alpha"); err != nil || !tn.Existing || tn.Status != "ready" {
		t.Errorf("alpha %+v %v", tn, err)
	}
	if len(prov.launches) != 0 {
		t.Errorf("provisioner ran: %v", prov.launches)
	}
}
