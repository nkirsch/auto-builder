package github

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
)

func TestHistoryCalls(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/o/r/commits":
			w.Write([]byte(`[{"sha":"b","commit":{"committer":{"date":"2026-10-27T10:00:00Z"}}},{"sha":"a","commit":{"committer":{"date":"2026-10-26T10:00:00Z"}}}]`))
		case "/repos/o/r/git/matching-refs/tags/v":
			w.Write([]byte(`[{"ref":"refs/tags/v26.10.27-1","object":{"sha":"b","type":"commit"}},{"ref":"refs/tags/v26.10.26-1","object":{"sha":"x","type":"tag"}}]`))
		case "/repos/o/r/pulls":
			if r.URL.Query().Get("state") != "closed" {
				t.Errorf("pulls query %s", r.URL.RawQuery)
			}
			w.Write([]byte(`[{"number":2,"title":"Two","merged_at":"2026-10-27T10:00:00Z","merge_commit_sha":"b","user":{"login":"bot"}},{"number":1,"title":"Dropped","merged_at":null}]`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c, ctx := New(srv.URL, ""), context.Background()
	commits, err := c.Commits(ctx, "o/r", 100)
	if err != nil || len(commits) != 2 || commits[0].SHA != "b" || commits[1].At.Day() != 26 {
		t.Fatalf("commits = %+v, %v", commits, err)
	}
	tags, err := c.CommitTags(ctx, "o/r", "v")
	if err != nil || len(tags) != 1 || !slices.Equal(tags["b"], []string{"v26.10.27-1"}) {
		t.Fatalf("tags = %v, %v (annotated tags skipped)", tags, err)
	}
	prs, err := c.MergedPRs(ctx, "o/r", 100)
	if err != nil || len(prs) != 1 || prs[0].Number != 2 || prs[0].SHA != "b" || prs[0].Author != "bot" {
		t.Fatalf("prs = %+v, %v", prs, err)
	}
}
