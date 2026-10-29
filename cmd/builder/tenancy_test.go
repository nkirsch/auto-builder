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
	"github.com/nkirsch/auto-builder/internal/db"
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
	if _, err := pool.Exec(ctx, `DROP SCHEMA IF EXISTS control, t_alpha, t_beta, t_jerrys, t_anns, t_carols, t_daves, t_erins CASCADE`); err != nil {
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
	check(do(h, srv, "mallory", "GET", "/new", nil), 403, "", "add you as a creator")
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
	// The app's traffic is for board admins; the test hub has no log project.
	check(do(h, srv, "bob", "GET", "/alpha/logs", nil), 403, "", "")
	check(do(h, srv, "nick@example.org", "GET", "/alpha/logs", nil), 502, "", "no request log view")
	if strings.Contains(do(h, srv, "nick@example.org", "GET", "/alpha/admin", nil).Body.String(), "Site traffic") {
		t.Error("admin links traffic without LOG_PROJECT")
	}
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

	// get_item's attachment URLs are served by the hub's runner API.
	h.storage.Put(ctx, "t/alpha/attachments/1/a.txt", "text/plain", strings.NewReader("hi"))
	alpha.AddAttachment(ctx, db.Attachment{ItemID: 1, Name: "a.txt", ContentType: "text/plain", Size: 2, Object: "attachments/1/a.txt", UploadedBy: "nkirsch"})
	w = runnerDo(srv, tok, "POST", "/mcp", `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"get_item","arguments":{"id":1}}}`)
	if !strings.Contains(w.Body.String(), `https://hub.example/api/runner/attachments/1`) {
		t.Fatalf("get_item attachment URL: %s", w.Body.String())
	}
	if a := runnerDo(srv, tok, "GET", "/api/runner/attachments/1", ""); a.Code != 200 || a.Body.String() != "hi" {
		t.Errorf("attachment: %d %q", a.Code, a.Body.String())
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

// A new app's board shows its setup's progress; once provisioned it's
// securing until it answers on its domain: its board runs, and the tick
// makes it ready once the app responds.
func TestHubSecuring(t *testing.T) {
	ctx := context.Background()
	h := testHub(t)
	job, prov := &fakeJob{}, &fakeJob{}
	h.launcher, h.provisioner = job, prov
	up := false
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !up { // no certificate yet: the connection fails
			if c, _, err := w.(http.Hijacker).Hijack(); err == nil {
				c.Close()
			}
			return
		}
		w.Write([]byte("hello"))
	}))
	defer app.Close()
	h.appBase = func(slug string) string { return app.URL + "/" + slug }
	srv := h.routes()
	if w := do(h, srv, "nkirsch", "POST", "/new", url.Values{"slug": {"alpha"}}); w.Code != 303 {
		t.Fatalf("create: %d", w.Code)
	}
	// Until the app is ready, its board shows the setup's progress and reloads.
	body := do(h, srv, "nick@example.org", "GET", "/alpha/", nil).Body.String()
	for _, want := range []string{`<meta http-equiv="refresh" content="15">`, "Setting up this app", `<progress max="100" value="9">`,
		`<li class="step-done">Job queued`, `<li class="step-pending">DNS and certificate`} {
		if !strings.Contains(body, want) {
			t.Errorf("provisioning board missing %q", want)
		}
	}
	// A failed step shows its error, and the board's admins can retry.
	h.reg.SetStep(ctx, "alpha", "Domain mapping", "failed", "domain not verified")
	h.reg.SetStatus(ctx, "alpha", "failed", "beta run: domain not verified")
	body = do(h, srv, "nick@example.org", "GET", "/alpha/", nil).Body.String()
	if !strings.Contains(body, `<li class="step-failed">Domain mapping <span class="muted">failed</span> <span class="bad">domain not verified</span>`) ||
		!strings.Contains(body, `action="/alpha/admin/provision"`) || strings.Contains(body, `http-equiv="refresh"`) {
		t.Errorf("failed provisioning board: %s", body)
	}
	if w := do(h, srv, "nick@example.org", "POST", "/alpha/admin/provision", url.Values{}); !strings.Contains(w.Header().Get("Location"), "Provisioning%20again") ||
		prov.launches[len(prov.launches)-1]["ACTION"] != "sync" {
		t.Fatalf("retry: %q %v", w.Header().Get("Location"), prov.launches)
	}
	if w := do(h, srv, "nick@example.org", "POST", "/alpha/admin/provision", url.Values{}); !strings.Contains(w.Header().Get("Location"), "didn") {
		t.Fatalf("retry while provisioning: %q", w.Header().Get("Location"))
	}

	h.reg.SetStatus(ctx, "alpha", "securing", "")
	if !strings.Contains(do(h, srv, "nick@example.org", "GET", "/alpha/", nil).Body.String(), "Waiting for the app") {
		t.Error("securing board without its panel")
	}
	if w := do(h, srv, "nick@example.org", "POST", "/alpha/admin/run", url.Values{}); !strings.Contains(w.Header().Get("Location"), "Started") {
		t.Fatalf("run now while securing: %q", w.Header().Get("Location"))
	}
	tick := func() {
		t.Helper()
		r := httptest.NewRequest("POST", "/api/gate/tick", nil)
		r.Header.Set("Authorization", "Bearer tick")
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatalf("tick: %d %s", w.Code, w.Body.String())
		}
	}
	tick()
	if tn, _ := h.reg.Tenant(ctx, "alpha"); tn.Status != "securing" || tn.ProvisionedAt != nil {
		t.Fatalf("unreachable app %+v", tn)
	}
	up = true
	tick()
	if tn, _ := h.reg.Tenant(ctx, "alpha"); tn.Status != "ready" || tn.ProvisionedAt == nil || tn.Steps[len(tn.Steps)-1].State != "done" {
		t.Fatalf("reachable app %+v", tn)
	}
	if body := do(h, srv, "nick@example.org", "GET", "/alpha/", nil).Body.String(); strings.Contains(body, `class="provisioning"`) || strings.Contains(body, "http-equiv") {
		t.Error("ready board still shows provisioning")
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
		d, err := h.deployed(ctx, tn)
		s := h.shipper(tn, st, nil, nil, d)
		if err != nil || s == nil || (s.GitSHA == "abc1234") != platform || (s.Deployed != nil) == platform || (d != nil) == platform {
			t.Errorf("%s shipper %+v %v", slug, s, err)
		}
	}

	// The footer's version and commit link the hub's repo, not the app's.
	w = do(h, srv, "nick@example.org", "GET", "/beta/", nil)
	if !strings.Contains(w.Body.String(), "github.com/kirsch/extreme/commit/abc1234") || strings.Contains(w.Body.String(), "someone/beta/commit") {
		t.Errorf("tenant footer: %s", w.Body.String())
	}
	// The nav's version beside the app's name would be the hub's, not the app's.
	if strings.Contains(w.Body.String(), `<span class="version"`) {
		t.Errorf("tenant nav shows the hub's version: %s", w.Body.String())
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

// Creators make one app each: from the creators list or as members of a
// CREATORS_FROM board; whoever admins an app already (here or on a SITES
// board) has theirs.
func TestHubCreators(t *testing.T) {
	ctx := context.Background()
	h := testHub(t)
	roles := map[string]map[string]string{
		"builder": {"bob": "member", "momo": "member"},
		"momo":    {"momo": "admin"},
	}
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer stats" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		site := strings.Trim(r.URL.Path[:strings.Index(r.URL.Path, "/api/")], "/")
		writeJSON(w, http.StatusOK, map[string]string{"role": roles[site][r.URL.Query().Get("identity")]})
	}))
	defer remote.Close()
	h.cfg.StatsToken = "stats"
	h.cfg.Sites = []remoteSite{{Name: "builder", URL: remote.URL + "/builder"}, {Name: "momo", URL: remote.URL + "/momo"}}
	h.cfg.CreatorsFrom = []string{"builder"}
	srv := h.routes()
	code := func(w *httptest.ResponseRecorder, want int) {
		t.Helper()
		if w.Code != want {
			t.Fatalf("%d %.300s; want %d", w.Code, w.Body.String(), want)
		}
	}

	code(do(h, srv, "jerry", "GET", "/new", nil), 403)
	if strings.Contains(do(h, srv, "jerry", "GET", "/", nil).Body.String(), `href="/new"`) {
		t.Error("New app offered to a non-creator")
	}
	code(do(h, srv, "nkirsch", "POST", "/admin/creators", url.Values{"identity": {"jerry"}}), 303)
	code(do(h, srv, "nkirsch", "GET", "/admin", nil), 200)
	if !strings.Contains(do(h, srv, "jerry", "GET", "/", nil).Body.String(), `href="/new"`) {
		t.Error("New app not offered to a creator")
	}
	w := do(h, srv, "jerry", "GET", "/new", nil)
	code(w, 200)
	if strings.Contains(w.Body.String(), `name="existing"`) {
		t.Error("a creator may pick an existing repo")
	}
	// A creator can't name the repo or bring an existing one.
	w = do(h, srv, "jerry", "POST", "/new", url.Values{"slug": {"jerrys"}, "name": {"Jerry's"}, "repo": {"evil/x"}, "existing": {"on"}})
	if w.Code != 303 || w.Header().Get("Location") != "/jerrys/" {
		t.Fatalf("create: %d %s", w.Code, w.Header().Get("Location"))
	}
	tn, err := h.reg.Tenant(ctx, "jerrys")
	if err != nil || tn.Repo != "extreme-dot-org/jerrys" || tn.Existing || tn.CreatedBy != "jerry" || tn.Name != "Jerry's" {
		t.Fatalf("jerrys %+v %v", tn, err)
	}
	st, _ := h.reg.Store(ctx, "jerrys")
	if m, err := st.Member(ctx, "jerry"); err != nil || m.Role != "admin" {
		t.Fatalf("jerry on his board: %+v %v", m, err)
	}
	code(do(h, srv, "jerry", "GET", "/jerrys/", nil), 200)
	code(do(h, srv, "jerry", "POST", "/new", url.Values{"slug": {"jerry2"}}), 403) // one app each

	// Members of builder may create; momo admins her own site, so she has hers.
	code(do(h, srv, "bob", "GET", "/new", nil), 200)
	code(do(h, srv, "momo", "GET", "/new", nil), 403)
	code(do(h, srv, "mallory", "GET", "/new", nil), 403)

	// A Google creator gets a board login from their address.
	code(do(h, srv, "nkirsch", "POST", "/admin/creators", url.Values{"identity": {"Ann.Lee@Example.org"}}), 303)
	code(do(h, srv, "ann.lee@example.org", "POST", "/new", url.Values{"slug": {"anns"}}), 303)
	st, _ = h.reg.Store(ctx, "anns")
	if m, err := st.MemberByEmail(ctx, "ann.lee@example.org"); err != nil || m.Role != "admin" || m.Login != "annlee" {
		t.Fatalf("ann on her board: %+v %v", m, err)
	}
	code(do(h, srv, "nkirsch", "POST", "/admin/creators/jerry/delete", url.Values{}), 303)
	if ok, _ := h.reg.Creator(ctx, "jerry"); ok {
		t.Error("jerry still a creator")
	}
}

func TestHubCreateForOwner(t *testing.T) {
	ctx := context.Background()
	h := testHub(t)
	srv := h.routes()

	if !strings.Contains(do(h, srv, "nkirsch", "GET", "/new", nil).Body.String(), `name="owner"`) {
		t.Error("New app has no owner field for platform admins")
	}
	// A platform admin makes carol's app: she's its admin, it's her one app.
	w := do(h, srv, "nkirsch", "POST", "/new", url.Values{"slug": {"carols"}, "name": {"Carol's"}, "owner": {"@carol"}})
	if w.Code != 303 || !strings.HasPrefix(w.Header().Get("Location"), "/carols/?flash=") {
		t.Fatalf("create: %d %s", w.Code, w.Header().Get("Location"))
	}
	tn, err := h.reg.Tenant(ctx, "carols")
	if err != nil || tn.CreatedBy != "carol" || tn.Repo != "extreme-dot-org/carols" {
		t.Fatalf("carols %+v %v", tn, err)
	}
	st, _ := h.reg.Store(ctx, "carols")
	for _, login := range []string{"carol", "nkirsch"} {
		if m, err := st.Member(ctx, login); err != nil || m.Role != "admin" {
			t.Fatalf("%s on carols: %+v %v", login, m, err)
		}
	}
	if !strings.Contains(do(h, srv, "carol", "GET", "/", nil).Body.String(), `href="/carols/"`) {
		t.Error("carol's app not on her index")
	}
	if w := do(h, srv, "carol", "GET", "/carols/", nil); w.Code != 200 {
		t.Errorf("carol on her board: %d", w.Code)
	}
	h.reg.AddCreator(ctx, "carol", "nkirsch")
	if ok, err := h.canCreate(ctx, "carol"); ok || err != nil {
		t.Errorf("carol can create another: %v %v", ok, err)
	}

	// An address owner signs in with Google.
	do(h, srv, "nkirsch", "POST", "/new", url.Values{"slug": {"daves"}, "owner": {"Dave@Example.org"}})
	if tn, err := h.reg.Tenant(ctx, "daves"); err != nil || tn.CreatedBy != "dave@example.org" {
		t.Fatalf("daves %+v %v", tn, err)
	}
	st, _ = h.reg.Store(ctx, "daves")
	if m, err := st.MemberByEmail(ctx, "dave@example.org"); err != nil || m.Role != "admin" || m.Login != "dave" {
		t.Fatalf("dave on his board: %+v %v", m, err)
	}

	// Only platform admins name an owner.
	h.reg.AddCreator(ctx, "erin", "nkirsch")
	do(h, srv, "erin", "POST", "/new", url.Values{"slug": {"erins"}, "owner": {"mallory"}})
	if tn, err := h.reg.Tenant(ctx, "erins"); err != nil || tn.CreatedBy != "erin" {
		t.Fatalf("erins %+v %v", tn, err)
	}
}

// Platform admins set the all-apps limits and each app's spend limit on the
// hub; an app's own admins only see theirs.
func TestHubSpendLimits(t *testing.T) {
	ctx := context.Background()
	h := testHub(t)
	h.launcher, h.provisioner = &fakeJob{}, &fakeJob{}
	srv := h.routes()
	if w := do(h, srv, "nkirsch", "POST", "/new", url.Values{"slug": {"alpha"}}); w.Code != 303 {
		t.Fatalf("create: %d", w.Code)
	}
	h.reg.SetStatus(ctx, "alpha", "ready", "")

	if w := do(h, srv, "nkirsch", "POST", "/admin/limits", url.Values{"limit": {"75"}, "max_runs": {"4"}}); w.Code != 303 {
		t.Fatalf("limits: %d", w.Code)
	}
	if set, _ := h.reg.Settings(ctx); set.SpendLimit != 75 || set.MaxRuns != 4 {
		t.Errorf("settings %+v", set)
	}
	if w := do(h, srv, "nkirsch", "POST", "/admin/limits", url.Values{"limit": {"0"}, "max_runs": {"4"}}); !strings.Contains(w.Header().Get("Location"), "Enter") {
		t.Errorf("bad limit accepted: %q", w.Header().Get("Location"))
	}
	if w := do(h, srv, "nkirsch", "POST", "/admin/tenants/alpha/spend-limit", url.Values{"limit": {"12.5"}}); w.Code != 303 {
		t.Fatalf("tenant limit: %d", w.Code)
	}
	st, _ := h.reg.Store(ctx, "alpha")
	if v, _ := st.SpendLimit(ctx); v != 12.5 {
		t.Errorf("alpha limit %v", v)
	}
	w := do(h, srv, "nkirsch", "GET", "/admin", nil)
	for _, want := range []string{`value="75.00"`, `value="12.50"`, `action="/admin/tenants/alpha/spend-limit"`, "<summary>Manage</summary>"} {
		if !strings.Contains(w.Body.String(), want) {
			t.Errorf("hub admin missing %s", want)
		}
	}

	// The app's own admin sees the limit but can't change it.
	if w := do(h, srv, "nick@example.org", "POST", "/alpha/admin/spend-limit", url.Values{"limit": {"500"}}); !strings.Contains(w.Header().Get("Location"), "hub") {
		t.Errorf("board set its own limit: %q", w.Header().Get("Location"))
	}
	if v, _ := st.SpendLimit(ctx); v != 12.5 {
		t.Errorf("alpha limit changed to %v", v)
	}
	if w := do(h, srv, "nick@example.org", "GET", "/alpha/admin", nil); !strings.Contains(w.Body.String(), "set by the hub") || strings.Contains(w.Body.String(), `action="/alpha/admin/spend-limit"`) {
		t.Errorf("board admin: %d %.300s", w.Code, w.Body.String())
	}
	if pct, lvl := spendLevel(9, 10); pct != 90 || lvl != "near" {
		t.Errorf("spendLevel(9, 10) = %d %q", pct, lvl)
	}
	if pct, lvl := spendLevel(12, 10); pct != 100 || lvl != "over" {
		t.Errorf("spendLevel(12, 10) = %d %q", pct, lvl)
	}
}

func TestBoardOf(t *testing.T) {
	for next, want := range map[string]string{"/alpha/items/3": "alpha", "/alpha": "alpha", "/": "", "/admin": "", "/Bad_Slug/x": ""} {
		if got := boardOf(next); got != want {
			t.Errorf("boardOf(%q) = %q, want %q", next, got, want)
		}
	}
}
