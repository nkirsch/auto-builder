package gate

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/nkirsch/auto-builder/internal/github"
)

func TestShippedNote(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/o/r/commits/abcdef123456":
			w.Write([]byte(`{"commit":{"committer":{"date":"2026-10-25T03:04:05Z"}}}`))
		case "/repos/o/r/commits":
			w.Write([]byte(`[{"sha":"abcdef123456"},{"sha":"def"}]`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	s := &Shipper{GitHub: github.New(srv.URL, ""), Repo: "o/r"}

	if got := s.shippedNote(context.Background(), "abcdef123456"); got != "Shipped in v26.10.25-2 (abcdef1)." {
		t.Fatalf("note = %q", got)
	}
	if got := s.shippedNote(context.Background(), "0123456789"); got != "Shipped in 0123456." {
		t.Fatalf("fallback note = %q", got)
	}
}
