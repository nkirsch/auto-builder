package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestSite(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "index.html"), []byte("home"), 0o644)
	os.WriteFile(filepath.Join(dir, "posts.html"), []byte("posts SITE_VERSION"), 0o644)
	os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("SITE_VERSION"), 0o644)
	app := http.NewServeMux()
	app.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("board")) })
	app.HandleFunc("GET /go", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/items/1", http.StatusFound) })
	version := ""
	h, err := site("https://site.example/iterate", dir, app, func() string { return version })
	if err != nil {
		t.Fatal(err)
	}
	get := func(host, path string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.Host = host
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	cases := []struct {
		host, path string
		code       int
		body, loc  string
	}{
		{"site.example", "/", 200, "home", ""},
		{"other.example", "/posts.html", 308, "", "https://site.example/posts.html"},
		{"localhost:8080", "/", 200, "home", ""},
		{"site.example", "/iterate/", 200, "board", ""},
		{"site.example", "/iterate", 301, "", "/iterate/"},
		{"site.example", "/iterate/go", 302, "", "/iterate/items/1"},
		{"other.example", "/iterate/go?x=1", 308, "", "https://site.example/iterate/go?x=1"},
	}
	for _, c := range cases {
		w := get(c.host, c.path)
		if w.Code != c.code || (c.body != "" && w.Body.String() != c.body) || w.Header().Get("Location") != c.loc {
			t.Errorf("%s%s: %d %q loc=%q", c.host, c.path, w.Code, w.Body.String(), w.Header().Get("Location"))
		}
	}

	// The footer's version marker is filled in on HTML pages once known.
	if w := get("site.example", "/posts.html"); w.Body.String() != "posts " {
		t.Errorf("before version: %q", w.Body.String())
	}
	version = "v26.10.25-2"
	if w := get("site.example", "/posts.html"); w.Body.String() != "posts v26.10.25-2" || w.Header().Get("Last-Modified") != "" {
		t.Errorf("with version: %q last-modified=%q", w.Body.String(), w.Header().Get("Last-Modified"))
	}
	if w := get("site.example", "/notes.txt"); w.Body.String() != "SITE_VERSION" {
		t.Errorf("non-HTML file changed: %q", w.Body.String())
	}
}

// Board only, behind a proxy that sends the origin's Host: no redirect.
func TestBoardOnlyBehindProxy(t *testing.T) {
	app := http.NewServeMux()
	app.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("board")) })
	app.HandleFunc("GET /go", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/items/1", http.StatusFound) })
	h, err := site("https://site.example/iterate", "", app, func() string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		path      string
		code      int
		body, loc string
	}{
		{"/iterate/", 200, "board", ""},
		{"/iterate/go", 302, "", "/iterate/items/1"},
		{"/", 404, "", ""},
	} {
		r := httptest.NewRequest(http.MethodGet, c.path, nil)
		r.Host = "svc.run.app"
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != c.code || (c.body != "" && w.Body.String() != c.body) || w.Header().Get("Location") != c.loc {
			t.Errorf("%s: %d %q loc=%q", c.path, w.Code, w.Body.String(), w.Header().Get("Location"))
		}
	}
}
