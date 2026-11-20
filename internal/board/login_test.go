package board

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/nkirsch/auto-builder/internal/auth"
	"github.com/nkirsch/auto-builder/internal/db"
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
		`<meta property="og:title" content="`,
		`<meta property="og:image" content="https://builder.example/static/og.png">`,
		`<meta property="og:url" content="https://builder.example/">`,
		`href="/auth/github?next=%2fitems%2f7"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %s", want)
		}
	}
	for _, leak := range []string{`href="/items/new"`, `action="/auth/logout"`, `action="/theme"`} {
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

func TestLoginPageOffersGoogle(t *testing.T) {
	a, err := auth.New(auth.Config{BaseURL: "https://builder.example", SessionKey: []byte("0123456789abcdef"), GoogleClientID: "g"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	b := &Board{Auth: a, BaseURL: "https://builder.example"}
	if err := b.parse(); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	b.login(w, httptest.NewRequest(http.MethodGet, "/auth/login?next=/items/7", nil))
	body := w.Body.String()
	if !strings.Contains(body, `href="/auth/google?next=%2fitems%2f7"`) || strings.Contains(body, `/auth/github`) {
		t.Fatalf("want Google only:\n%s", body)
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

// Google members get Connect GitHub in the menu once the GitHub app is
// configured, or their connected account with Disconnect.
func TestConnectGitHubMenu(t *testing.T) {
	b, _ := dbBoard(t)
	a, err := auth.New(auth.Config{BaseURL: "https://builder.example", SessionKey: []byte("0123456789abcdef"),
		ClientID: "gh", GoogleClientID: "g", GitHubLinkOnly: true}, b.Store)
	if err != nil {
		t.Fatal(err)
	}
	b.Auth = a
	if err := b.parse(); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		me   db.Member
		want string
	}{
		{db.Member{Login: "kat", Email: "kat@example.com"}, `href="/auth/github/link?next=%2fruns"`},
		{db.Member{Login: "kat", Email: "kat@example.com", GitHubLogin: "kat-gh"}, `GitHub @kat-gh`},
		{db.Member{Login: "octocat"}, ""},
	} {
		w := httptest.NewRecorder()
		b.runs(w, httptest.NewRequest(http.MethodGet, "/runs", nil).WithContext(auth.WithMember(context.Background(), c.me)))
		body := w.Body.String()
		if c.want == "" {
			if strings.Contains(body, "/auth/github/") {
				t.Errorf("%s: GitHub link offered to a GitHub member", c.me.Login)
			}
		} else if !strings.Contains(body, c.want) {
			t.Errorf("%+v: missing %s", c.me, c.want)
		}
	}
}

func TestLoginPageSharePreview(t *testing.T) {
	b, _ := dbBoard(t)
	a, err := auth.New(auth.Config{BaseURL: b.BaseURL, SessionKey: []byte("0123456789abcdef")}, nil)
	if err != nil {
		t.Fatal(err)
	}
	b.Auth = a
	if err := b.parse(); err != nil {
		t.Fatal(err)
	}
	id, err := b.Store.CreateItem(context.Background(), "Match the homepage", "Like if you click on about me it goes back to the old style", "momo")
	if err != nil {
		t.Fatal(err)
	}
	share := b.shareURL(id)
	tok := share[strings.Index(share, "?s=")+3:]
	get := func(next string) string {
		w := httptest.NewRecorder()
		b.login(w, httptest.NewRequest(http.MethodGet, "/auth/login?next="+url.QueryEscape(next), nil))
		return w.Body.String()
	}
	body := get(fmt.Sprintf("/items/%d?s=%s", id, tok))
	for _, want := range []string{
		`<meta property="og:title" content="Match the homepage">`,
		`<meta property="og:description" content="Like if you click on about me it goes back to the old style">`,
		`<meta property="og:url" content="` + share + `">`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %s", want)
		}
	}
	for _, next := range []string{fmt.Sprintf("/items/%d", id), fmt.Sprintf("/items/%d?s=0000000000000000", id), fmt.Sprintf("/items/%d?s=%s", id+1, tok)} {
		if strings.Contains(get(next), "Match the homepage") {
			t.Errorf("%s leaks the title", next)
		}
	}
}

func TestExcerpt(t *testing.T) {
	if got := excerpt("a\n\nb  c", 10); got != "a b c" {
		t.Errorf("got %q", got)
	}
	if got := excerpt("one two three four", 12); got != "one two…" {
		t.Errorf("got %q", got)
	}
}
