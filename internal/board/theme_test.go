package board

import (
	"html/template"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/nkirsch/auto-builder/internal/db"
	"github.com/nkirsch/auto-builder/web"
)

func TestBackPath(t *testing.T) {
	for in, want := range map[string]string{
		"":                     "/",
		"/":                    "/",
		"/items/3":             "/items/3",
		"/items/3?m=Replied":   "/items/3",
		"/runs?page=2&m=x":     "/runs?page=2",
		"//evil.example/":      "/",
		"/\\evil.example":      "/",
		"https://evil.example": "/",
		"items/3":              "/",
	} {
		if got := backPath(in); got != want {
			t.Errorf("backPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSetTheme(t *testing.T) {
	b := &Board{BaseURL: "https://builder.example"}
	post := func(form url.Values) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/theme", strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		b.setTheme(w, r)
		return w
	}

	w := post(url.Values{"theme": {"dark"}, "back": {"/items/1?m=hi"}})
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/items/1" {
		t.Fatalf("got %d to %q", w.Code, w.Header().Get("Location"))
	}
	cs := w.Result().Cookies()
	if len(cs) != 1 || cs[0].Name != themeCookie || cs[0].Value != "dark" || !cs[0].Secure || !cs[0].HttpOnly {
		t.Fatalf("cookies = %+v", cs)
	}

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.AddCookie(cs[0])
	if got := theme(r); got != "dark" {
		t.Fatalf("theme = %q", got)
	}
	r = httptest.NewRequest(http.MethodGet, "/", nil)
	r.AddCookie(&http.Cookie{Name: themeCookie, Value: `x" onload="`})
	if got := theme(r); got != "" {
		t.Fatalf("bogus theme cookie honoured: %q", got)
	}

	if w := post(url.Values{"theme": {"neon"}}); w.Code != http.StatusBadRequest {
		t.Fatalf("bad theme: got %d", w.Code)
	}
}

func TestLayoutTheme(t *testing.T) {
	b := &Board{}
	if err := b.parse(); err != nil {
		t.Fatal(err)
	}
	var sb strings.Builder
	p := page{Me: db.Member{Login: "alice"}, Title: "x", Theme: "dark", Path: "/items/1"}
	if err := b.tmpl["new"].ExecuteTemplate(&sb, "layout", p); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`<html lang="en" data-theme="dark">`, `action="/theme"`, `name="back" value="/items/1"`} {
		if !strings.Contains(sb.String(), want) {
			t.Errorf("layout missing %s", want)
		}
	}
}

func TestLayoutColorScheme(t *testing.T) {
	b := &Board{}
	if err := b.parse(); err != nil {
		t.Fatal(err)
	}
	for th, want := range map[string]string{"": "light dark", "light": "only light", "dark": "dark"} {
		var sb strings.Builder
		p := page{Me: db.Member{Login: "alice"}, Theme: th, GitSHA: "abc1234def"}
		if err := b.tmpl["new"].ExecuteTemplate(&sb, "layout", p); err != nil {
			t.Fatal(err)
		}
		if tag := `<meta name="color-scheme" content="` + want + `">`; !strings.Contains(sb.String(), tag) {
			t.Errorf("theme %q: layout missing %s", th, tag)
		}
		if !strings.Contains(sb.String(), `href="/static/style.css?v=abc1234"`) {
			t.Errorf("theme %q: stylesheet not versioned", th)
		}
	}
}

func TestLayoutVersion(t *testing.T) {
	b := &Board{GitSHA: "abc123"}
	if err := b.parse(); err != nil {
		t.Fatal(err)
	}
	layout := func() string {
		p := page{Me: db.Member{Login: "alice"}, GitSHA: b.GitSHA}
		if v := b.version.Load(); v != nil {
			p.Version = *v
		}
		var sb strings.Builder
		if err := b.tmpl["new"].ExecuteTemplate(&sb, "layout", p); err != nil {
			t.Fatal(err)
		}
		return sb.String()
	}
	if strings.Contains(layout(), `class="version"`) {
		t.Fatal("version shown before it is known")
	}
	b.SetVersion("v26.10.25-2")
	if want := `<span class="version" title="abc123">v26.10.25-2</span></a>`; !strings.Contains(layout(), want) {
		t.Fatalf("layout missing %s", want)
	}
}

func TestLayoutFooterCommitLink(t *testing.T) {
	b := &Board{}
	if err := b.parse(); err != nil {
		t.Fatal(err)
	}
	for repo, want := range map[string]string{
		"nkirsch/auto-builder": `<footer><a href="https://github.com/nkirsch/auto-builder/commit/abc1234def" title="abc1234def">abc1234</a>`,
		"":                     `<footer>abc1234`,
	} {
		var sb strings.Builder
		p := page{Me: db.Member{Login: "alice"}, GitSHA: "abc1234def", Repo: repo}
		if err := b.tmpl["new"].ExecuteTemplate(&sb, "layout", p); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(sb.String(), want) {
			t.Errorf("repo %q: layout missing %s", repo, want)
		}
	}
}

func TestLayoutFooterTagLink(t *testing.T) {
	b := &Board{}
	if err := b.parse(); err != nil {
		t.Fatal(err)
	}
	var sb strings.Builder
	p := page{Me: db.Member{Login: "alice"}, GitSHA: "abc1234def", Repo: "nkirsch/auto-builder", Version: "v26.10.25-3"}
	if err := b.tmpl["new"].ExecuteTemplate(&sb, "layout", p); err != nil {
		t.Fatal(err)
	}
	if want := `<footer><a href="https://github.com/nkirsch/auto-builder/tree/v26.10.25-3">v26.10.25-3</a> · <a href="https://github.com/nkirsch/auto-builder/commit/abc1234def"`; !strings.Contains(sb.String(), want) {
		t.Errorf("layout missing %s", want)
	}
}

func TestLayoutLinksUnderPath(t *testing.T) {
	b := &Board{BaseURL: "https://site.example/iterate"}
	if err := b.parse(); err != nil {
		t.Fatal(err)
	}
	var sb strings.Builder
	if err := b.tmpl["new"].ExecuteTemplate(&sb, "layout", page{Me: db.Member{Login: "alice"}}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`href="/iterate/static/style.css"`, `href="/iterate/items/new"`, `action="/iterate/auth/logout"`} {
		if !strings.Contains(sb.String(), want) {
			t.Errorf("layout missing %s", want)
		}
	}
}

func TestLayoutBrand(t *testing.T) {
	b := &Board{}
	if err := b.parse(); err != nil {
		t.Fatal(err)
	}
	tmpl := b.tmpl["new"]
	brand := func(name string) string {
		var sb strings.Builder
		if err := tmpl.ExecuteTemplate(&sb, name, page{}); err != nil {
			t.Fatal(err)
		}
		return sb.String()
	}
	var sb strings.Builder
	if err := tmpl.ExecuteTemplate(&sb, "layout", page{Title: "New", Me: db.Member{Login: "alice"}}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"<title>New · " + brand("brand-site") + "</title>",
		`class="brand">` + brand("brand-name") + "</a>" + brand("brand-nav"),
		brand("brand-head"),
		brand("brand-footer") + "</footer>",
		`href="/static/brand.css"`,
	} {
		if !strings.Contains(sb.String(), want) {
			t.Errorf("layout missing %s", want)
		}
	}
	// The NICK LLC bar is upstream's branding; forks choose their own footer.
	if brand("brand-site") == "auto-builder" && !strings.Contains(brand("brand-footer"), "llc.nick.org") {
		t.Error("brand-footer missing the NICK LLC link")
	}
}

// A fork's brand.html from before the brand-footer hook still renders.
func TestLayoutBrandFooterDefault(t *testing.T) {
	lay, err := fs.ReadFile(web.FS, "templates/layout.html")
	if err != nil {
		t.Fatal(err)
	}
	brand := `{{define "brand-site"}}s{{end}}{{define "brand-og-title"}}{{end}}{{define "brand-description"}}{{end}}{{define "brand-head"}}{{end}}{{define "brand-name"}}{{end}}{{define "brand-nav"}}{{end}}{{define "brand-signed-out"}}{{end}}{{define "content"}}{{end}}`
	tmpl, err := template.New("").Funcs(funcs).Funcs(template.FuncMap{"base": func() string { return "" }}).Parse(string(lay))
	if err == nil {
		_, err = tmpl.Parse(brand)
	}
	if err != nil {
		t.Fatal(err)
	}
	var sb strings.Builder
	if err := tmpl.ExecuteTemplate(&sb, "layout", page{Title: "New"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sb.String(), "</footer>") {
		t.Error("layout missing footer")
	}
}

// The login in the nav only reveals the sign-out button; it doesn't sign out.
func TestLayoutSignOutConfirms(t *testing.T) {
	b := &Board{}
	if err := b.parse(); err != nil {
		t.Fatal(err)
	}
	var sb strings.Builder
	if err := b.tmpl["new"].ExecuteTemplate(&sb, "layout", page{Me: db.Member{Login: "alice"}}); err != nil {
		t.Fatal(err)
	}
	out := sb.String()
	for _, want := range []string{`<summary title="Sign out…">alice ⎋</summary>`, `<button class="small">Sign out</button>`} {
		if !strings.Contains(out, want) {
			t.Errorf("layout missing %s", want)
		}
	}
	if strings.Contains(out, `<button class="link" title="Sign out">alice`) {
		t.Error("the login is still a sign-out button")
	}
}
