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
		AdminLogin: "nkirsch", AdminEmail: "nick@example.org",
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

	// Cross-site POSTs are refused.
	r := httptest.NewRequest("POST", "/new", strings.NewReader("slug=gamma"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", "https://evil.example")
	r.AddCookie(h.auth.SessionCookie("nkirsch"))
	w = httptest.NewRecorder()
	srv.ServeHTTP(w, r)
	check(w, 403, "", "cross-origin")
}
