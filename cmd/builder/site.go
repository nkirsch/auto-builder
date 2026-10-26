package main

import (
	"bytes"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
)

// site mounts the app at BASE_URL's path (e.g. /iterate) and, when siteDir
// is set, serves the static site from it at everything else, and other hosts
// redirect to BASE_URL's host, the canonical one. Without siteDir the board
// sits behind a proxy (CloudFront) that sends the origin's own Host, so no
// redirect.
// version gives the release name written into the site's HTML pages.
func site(baseURL, siteDir string, app http.Handler, version func() string) (http.Handler, error) {
	u, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("BASE_URL: %w", err)
	}
	prefix := strings.TrimSuffix(u.Path, "/")
	if prefix == "" {
		return app, nil
	}
	mux := http.NewServeMux()
	if siteDir != "" {
		mux.Handle("/", http.FileServer(versionFS{http.Dir(siteDir), version}))
	}
	mounted := http.StripPrefix(prefix, prefixLocation(prefix, app))
	mux.Handle(prefix+"/", mounted)
	mux.Handle(prefix, http.RedirectHandler(prefix+"/", http.StatusMovedPermanently))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if siteDir != "" && r.Host != u.Host && !local(r.Host) && !strings.HasSuffix(r.URL.Path, "/health") {
			to := *r.URL
			to.Scheme, to.Host = u.Scheme, u.Host
			http.Redirect(w, r, to.String(), http.StatusPermanentRedirect)
			return
		}
		mux.ServeHTTP(w, r)
	}), nil
}

// redirectHosts 308s requests for the listed hosts to the same path on
// BASE_URL's host, so a board can move domains without breaking old links.
func redirectHosts(baseURL string, hosts []string, next http.Handler) http.Handler {
	u, err := url.Parse(baseURL)
	if err != nil || len(hosts) == 0 {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if slices.Contains(hosts, r.Host) && r.Host != u.Host && !strings.HasSuffix(r.URL.Path, "/health") {
			to := *r.URL
			to.Scheme, to.Host = u.Scheme, u.Host
			http.Redirect(w, r, to.String(), http.StatusPermanentRedirect)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func local(host string) bool {
	h := host
	if i := strings.LastIndex(h, ":"); i >= 0 {
		h = h[:i]
	}
	return h == "localhost" || h == "127.0.0.1"
}

// prefixLocation puts prefix in front of the app's root-relative redirects.
func prefixLocation(prefix string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(&locationWriter{ResponseWriter: w, prefix: prefix}, r)
	})
}

type locationWriter struct {
	http.ResponseWriter
	prefix string
}

func (w *locationWriter) WriteHeader(code int) {
	if l := w.Header().Get("Location"); strings.HasPrefix(l, "/") && !strings.HasPrefix(l, "//") {
		w.Header().Set("Location", w.prefix+l)
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *locationWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// versionMarker is the placeholder in the site's footer (layouts/partials/foot.html).
const versionMarker = "SITE_VERSION"

// versionFS serves .html files with versionMarker replaced by the release
// name, or by nothing until it is known.
type versionFS struct {
	http.FileSystem
	version func() string
}

func (v versionFS) Open(name string) (http.File, error) {
	f, err := v.FileSystem.Open(name)
	if err != nil || !strings.HasSuffix(name, ".html") {
		return f, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if info.IsDir() {
		return v.FileSystem.Open(name)
	}
	b, err := io.ReadAll(f)
	if err != nil {
		return nil, err
	}
	b = bytes.ReplaceAll(b, []byte(versionMarker), []byte(v.version()))
	return &memFile{Reader: bytes.NewReader(b), info: info, size: int64(len(b))}, nil
}

// memFile is a substituted .html file. It reports no modification time, so
// browsers don't keep a copy that predates the version being known.
type memFile struct {
	*bytes.Reader
	info fs.FileInfo
	size int64
}

func (f *memFile) Close() error                       { return nil }
func (f *memFile) Readdir(int) ([]fs.FileInfo, error) { return nil, fs.ErrInvalid }
func (f *memFile) Stat() (fs.FileInfo, error)         { return memInfo{f.info, f.size}, nil }

type memInfo struct {
	fs.FileInfo
	size int64
}

func (i memInfo) Size() int64        { return i.size }
func (i memInfo) ModTime() time.Time { return time.Time{} }
