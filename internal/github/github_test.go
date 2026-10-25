package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
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
