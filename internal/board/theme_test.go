package board

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/nkirsch/auto-builder/internal/db"
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
	if want := `auto-builder <span class="version" title="abc123">v26.10.25-2</span></a>`; !strings.Contains(layout(), want) {
		t.Fatalf("layout missing %s", want)
	}
}
