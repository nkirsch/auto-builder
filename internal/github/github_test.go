package github

import (
	"context"
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
