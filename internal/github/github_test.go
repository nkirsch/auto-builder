package github

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestVersion(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/o/r/commits/abc":
			w.Write([]byte(`{"sha":"abc","commit":{"committer":{"date":"2026-10-25T03:04:05Z"}}}`))
		case "/repos/o/r/commits":
			q := r.URL.Query()
			if q.Get("sha") != "abc" || q.Get("since") != "2026-10-25T00:00:00Z" {
				t.Errorf("query = %v", q)
			}
			w.Write([]byte(`[{"sha":"abc"},{"sha":"def"},{"sha":"123"}]`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	v, err := New(srv.URL, "").Version(context.Background(), "o/r", "abc")
	if err != nil {
		t.Fatal(err)
	}
	if v != "v26.10.25-3" {
		t.Fatalf("version = %q", v)
	}
	if _, err := New(srv.URL, "").Version(context.Background(), "o/r", "nope"); !IsNotFound(err) {
		t.Fatalf("missing commit: err = %v", err)
	}
}

func TestChecksFailed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/o/r/commits/red/check-runs":
			w.Write([]byte(`{"check_runs":[{"conclusion":"success"},{"conclusion":"failure"}]}`))
		case "/repos/o/r/commits/pending/check-runs":
			w.Write([]byte(`{"check_runs":[{"conclusion":null},{"conclusion":"success"}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := New(srv.URL, "")
	for sha, want := range map[string]bool{"red": true, "pending": false} {
		got, err := c.ChecksFailed(context.Background(), "o/r", sha)
		if err != nil || got != want {
			t.Errorf("%s: got %v, %v; want %v", sha, got, err, want)
		}
	}
}

func TestTag(t *testing.T) {
	var created []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/repos/o/r/git/refs":
			var in struct{ Ref, SHA string }
			json.NewDecoder(r.Body).Decode(&in)
			if in.Ref != "refs/tags/v1" && in.Ref != "refs/tags/v2" {
				w.WriteHeader(http.StatusCreated)
				created = append(created, in.Ref+"@"+in.SHA)
				return
			}
			w.WriteHeader(http.StatusUnprocessableEntity)
			w.Write([]byte(`{"message":"Reference already exists"}`))
		case r.URL.Path == "/repos/o/r/git/ref/tags/v1":
			w.Write([]byte(`{"object":{"sha":"abc"}}`))
		case r.URL.Path == "/repos/o/r/git/ref/tags/v2":
			w.Write([]byte(`{"object":{"sha":"other"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := New(srv.URL, "")
	if err := c.Tag(context.Background(), "o/r", "v3", "abc"); err != nil || len(created) != 1 || created[0] != "refs/tags/v3@abc" {
		t.Fatalf("new tag: err = %v, created = %v", err, created)
	}
	if err := c.Tag(context.Background(), "o/r", "v1", "abc"); err != nil {
		t.Fatalf("existing tag on same commit: %v", err)
	}
	if err := c.Tag(context.Background(), "o/r", "v2", "abc"); err == nil {
		t.Fatal("existing tag on another commit: want error")
	}
}

func TestOpenIssuesPages(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if r.URL.Path != "/repos/o/r/issues" || q.Get("state") != "open" || q.Get("sort") != "updated" ||
			q.Get("since") != "2026-10-25T01:02:03Z" {
			t.Errorf("request = %s", r.URL)
		}
		n := 100
		if q.Get("page") == "2" {
			n = 3
		}
		list := make([]map[string]any, n)
		for i := range list {
			list[i] = map[string]any{"number": i + 1}
		}
		json.NewEncoder(w).Encode(list)
	}))
	defer srv.Close()
	since, _ := time.Parse(time.RFC3339, "2026-10-25T01:02:03Z")
	issues, err := New(srv.URL, "").OpenIssues(context.Background(), "o/r", since)
	if err != nil || len(issues) != 103 {
		t.Fatalf("issues = %d, err = %v", len(issues), err)
	}
}

func TestIssueCommentNumber(t *testing.T) {
	if n := (IssueComment{IssueURL: "https://api.github.com/repos/o/r/issues/42"}).IssueNumber(); n != 42 {
		t.Fatalf("number = %d", n)
	}
	if n := (IssueComment{}).IssueNumber(); n != 0 {
		t.Fatalf("empty number = %d", n)
	}
}

func TestReleaseCalls(t *testing.T) {
	var created map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/repos/o/r/releases/latest":
			w.Write([]byte(`{"tag_name":"v26.10.23-2"}`))
		case r.URL.Path == "/repos/o/r/commits/HEAD":
			w.Write([]byte(`{"sha":"head1"}`))
		case r.URL.Path == "/repos/o/r/compare/v26.10.23-2...head1":
			w.Write([]byte(`{"ahead_by":3}`))
		case r.URL.Path == "/repos/o/r/git/matching-refs/tags/v26.10.24":
			w.Write([]byte(`[{"ref":"refs/tags/v26.10.24"},{"ref":"refs/tags/v26.10.24-1"}]`))
		case r.URL.Path == "/repos/o/r/releases" && r.Method == http.MethodPost:
			json.NewDecoder(r.Body).Decode(&created)
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(`{"html_url":"https://github.com/o/r/releases/tag/v26.10.24-2"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c, ctx := New(srv.URL, ""), context.Background()

	latest, err := c.LatestRelease(ctx, "o/r")
	if err != nil || latest != "v26.10.23-2" {
		t.Fatalf("LatestRelease = %q, %v", latest, err)
	}
	head, ahead, err := c.Unreleased(ctx, "o/r", latest)
	if err != nil || head != "head1" || ahead != 3 {
		t.Fatalf("Unreleased = %q, %d, %v", head, ahead, err)
	}
	tags, err := c.TagsWithPrefix(ctx, "o/r", "v26.10.24")
	if err != nil || len(tags) != 2 || tags[1] != "v26.10.24-1" {
		t.Fatalf("TagsWithPrefix = %v, %v", tags, err)
	}
	if _, err := c.CreateRelease(ctx, "o/r", "v26.10.24-2", head); err != nil {
		t.Fatal(err)
	}
	if created["tag_name"] != "v26.10.24-2" || created["target_commitish"] != "head1" || created["generate_release_notes"] != true {
		t.Errorf("created = %v", created)
	}
	if latest, err := New(srv.URL, "").LatestRelease(ctx, "o/none"); err != nil || latest != "" {
		t.Errorf("no releases: %q, %v", latest, err)
	}
}

func TestBotIdentityAuthenticatedAndCached(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	users := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/app":
			w.Write([]byte(`{"slug":"bot"}`))
		case "/app/installations/9/access_tokens":
			w.Write([]byte(`{"token":"inst","expires_at":"2099-01-01T00:00:00Z"}`))
		case "/users/bot[bot]":
			users++
			if got := r.Header.Get("Authorization"); got != "Bearer inst" {
				t.Errorf("users auth = %q", got)
			}
			w.Write([]byte(`{"id":42}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	a := &App{ID: "1", InstallationID: "9", key: key, API: srv.URL, client: srv.Client()}
	for range 2 {
		name, email, err := a.BotIdentity(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if name != "bot[bot]" || email != "42+bot[bot]@users.noreply.github.com" {
			t.Fatalf("identity = %q, %q", name, email)
		}
	}
	if users != 1 {
		t.Fatalf("user lookups = %d, want 1", users)
	}
}
