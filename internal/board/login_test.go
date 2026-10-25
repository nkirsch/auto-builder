package board

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nkirsch/auto-builder/internal/auth"
)

func loginBoard(t *testing.T, base, devLogin string) *Board {
	t.Helper()
	a, err := auth.New(auth.Config{BaseURL: base, SessionKey: []byte("0123456789abcdef"), DevLogin: devLogin}, nil)
	if err != nil {
		t.Fatal(err)
	}
	b := &Board{Auth: a, BaseURL: base}
	if err := b.parse(); err != nil {
		t.Fatal(err)
	}
	return b
}

func TestLoginPagePreview(t *testing.T) {
	b := loginBoard(t, "https://builder.example", "")
	w := httptest.NewRecorder()
	b.login(w, httptest.NewRequest(http.MethodGet, "/auth/login?next=/items/7", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		`<meta property="og:title" content="auto-builder">`,
		`<meta property="og:image" content="https://builder.example/static/og.png">`,
		`<meta property="og:url" content="https://builder.example/">`,
		`href="/auth/github?next=%2fitems%2f7"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %s", want)
		}
	}
	for _, leak := range []string{`href="/inbox"`, `action="/auth/logout"`, `action="/theme"`} {
		if strings.Contains(body, leak) {
			t.Errorf("signed-out page shows member nav: %s", leak)
		}
	}
}

func TestLoginPageRejectsOffsiteNext(t *testing.T) {
	b := loginBoard(t, "https://builder.example", "")
	w := httptest.NewRecorder()
	b.login(w, httptest.NewRequest(http.MethodGet, "/auth/login?next=//evil.example", nil))
	if !strings.Contains(w.Body.String(), `href="/auth/github?next=%2f"`) {
		t.Fatal("offsite next not reset to /")
	}
}

func TestLoginPageRedirectsWhenSignedIn(t *testing.T) {
	b := loginBoard(t, "http://localhost:8080", "nkirsch")
	w := httptest.NewRecorder()
	b.login(w, httptest.NewRequest(http.MethodGet, "/auth/login?next=/runs", nil))
	if w.Code != http.StatusFound || w.Header().Get("Location") != "/runs" {
		t.Fatalf("got %d %q", w.Code, w.Header().Get("Location"))
	}
}
