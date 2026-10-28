package github

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestNextTag(t *testing.T) {
	for _, c := range []struct {
		existing []string
		want     string
	}{
		{nil, "v26.10.24-1"},
		{[]string{"v26.10.24"}, "v26.10.24-1"},
		{[]string{"v26.10.24", "v26.10.24-1", "v26.10.24-3"}, "v26.10.24-4"},
		{[]string{"v26.10.24-beta.2"}, "v26.10.24-1"},
		{[]string{"v26.10.240-1"}, "v26.10.24-1"}, // another day's prefix match
	} {
		if got := NextTag("v26.10.24", c.existing); got != c.want {
			t.Errorf("NextTag(%v) = %s, want %s", c.existing, got, c.want)
		}
	}
}

func TestReleaseDayPacific(t *testing.T) {
	// 2026-10-25 03:00 UTC is still Oct 24 in Pacific time.
	if got := ReleaseDay(time.Date(2026, 10, 25, 3, 0, 0, 0, time.UTC)); got != "v26.10.24" {
		t.Errorf("ReleaseDay = %s, want v26.10.24", got)
	}
	if got := ReleaseDay(time.Date(2026, 3, 5, 20, 0, 0, 0, time.UTC)); got != "v26.3.5" {
		t.Errorf("ReleaseDay = %s, want v26.3.5", got)
	}
}

func TestReleaseHead(t *testing.T) {
	var created string
	ahead := 2
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "GET /repos/o/r/releases/latest":
			w.Write([]byte(`{"tag_name":"v26.10.24-1"}`))
		case "GET /repos/o/r/commits/HEAD":
			w.Write([]byte(`{"sha":"abc"}`))
		case "GET /repos/o/r/compare/v26.10.24-1...abc":
			fmt.Fprintf(w, `{"ahead_by":%d}`, ahead)
		case "GET /repos/o/r/git/matching-refs/tags/v26.10.24":
			w.Write([]byte(`[{"ref":"refs/tags/v26.10.24-1"}]`))
		case "POST /repos/o/r/releases":
			b, _ := io.ReadAll(r.Body)
			created = string(b)
			w.Write([]byte(`{"html_url":"u"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := New(srv.URL, "t")
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, pacific)
	tag, n, err := c.ReleaseHead(context.Background(), "o/r", now)
	if err != nil || tag != "v26.10.24-2" || n != 2 || !strings.Contains(created, `"target_commitish":"abc"`) {
		t.Fatalf("tag=%q n=%d err=%v created=%s", tag, n, err, created)
	}
	ahead, created = 0, ""
	if tag, _, err := c.ReleaseHead(context.Background(), "o/r", now); err != nil || tag != "" || created != "" {
		t.Fatalf("nothing to release: tag=%q err=%v created=%s", tag, err, created)
	}
}
