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

// TestUrgentRelease: on a repo that deploys on release, merging an #urgent
// item's PR cuts a release; other merges wait for Release now.
func TestUrgentRelease(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/o/r/pulls/1":
			w.Write([]byte(`{"number":1,"state":"closed","merged":true,"merge_commit_sha":"aaa"}`))
		case "/repos/o/r/pulls/2":
			w.Write([]byte(`{"number":2,"state":"closed","merged":true,"merge_commit_sha":"bbb"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	plain, _ := st.CreateItem(ctx, "Tweak", "", "alice")
	urgent, _ := st.CreateItem(ctx, "Site down", "", "alice")
	st.SetTags(ctx, urgent, []string{"bug", UrgentTag}, false)
	st.SetPR(ctx, plain, 1)
	st.SetPR(ctx, urgent, 2)

	releases := 0
	s := &Shipper{Store: st, GitHub: github.New(srv.URL, ""), Repo: "o/r",
		Release: func(context.Context) error { releases++; return nil }}
	if err := s.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if releases != 1 {
		t.Fatalf("releases = %d, want 1 (the urgent item's)", releases)
	}
	for _, id := range []int{plain, urgent} {
		if it, _ := st.Item(ctx, id); it.Status != "merged" {
			t.Fatalf("item %d status = %q", id, it.Status)
		}
	}
	// Already merged: a later tick doesn't release again.
	if err := s.Check(ctx); err != nil || releases != 1 {
		t.Fatalf("second check: releases = %d, %v", releases, err)
	}
}
