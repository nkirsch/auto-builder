package gate

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nkirsch/auto-builder/internal/db"
	"github.com/nkirsch/auto-builder/internal/github"
)

// TestSyncSoon needs TEST_DATABASE_URL (a disposable database; tables are truncated).
func TestSyncSoon(t *testing.T) {
	u := os.Getenv("TEST_DATABASE_URL")
	if u == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	pool, err := db.Open(ctx, u)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if _, err := pool.Exec(ctx, `TRUNCATE members, items, responses, attachments, notifications, runs RESTART IDENTITY CASCADE`); err != nil {
		t.Fatal(err)
	}
	st := &db.Store{Pool: pool}
	id, err := st.CreateItem(ctx, "Version in footer", "", "nkirsch")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetPR(ctx, id, 5); err != nil {
		t.Fatal(err)
	}
	if err := st.SetItemIssue(ctx, id, 7); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddResponse(ctx, id, "builder-bot", "Opened PR #5.", false); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var comments []string

	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/o/r/pulls/5":
			w.Write([]byte(`{"number":5,"state":"closed","merged":true,"merge_commit_sha":"abcdef123456"}`))
		case "/repos/o/r/compare/deployed...abcdef123456":
			w.Write([]byte(`{"status":"behind"}`))
		case "/repos/o/r/issues", "/repos/o/r/issues/comments":
			w.Write([]byte(`[]`))
		case "/repos/o/r/issues/7/comments":
			var m map[string]string
			json.NewDecoder(r.Body).Decode(&m)
			mu.Lock()
			comments = append(comments, m["body"])
			mu.Unlock()
			w.Write([]byte(`{"id":1}`))
		case "/repos/o/r/issues/7":
			w.Write([]byte(`{}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer gh.Close()

	var replies []string
	g := &Gate{Store: st, Issues: &IssueSync{Store: st, GitHub: github.New(gh.URL, ""), Repo: "o/r", BaseURL: "https://b"}, Ship: &Shipper{
		Store: st, GitHub: github.New(gh.URL, ""), Repo: "o/r", GitSHA: "deployed",
		Reply: func(_ context.Context, _ db.Item, body string) error {
			mu.Lock()
			defer mu.Unlock()
			replies = append(replies, body)
			return nil
		},
	}}
	g.SyncSoon()

	deadline := time.Now().Add(5 * time.Second)
	for {
		it, err := st.Item(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		mu.Lock()
		replied := len(replies) > 0
		mu.Unlock()
		// The reply lands just after the status; wait for both.
		if it.Status == "shipped" && replied {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("status = %q, want shipped", it.Status)
		}
		time.Sleep(20 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(replies) != 1 || replies[0] != "Shipped in abcdef1." {
		t.Fatalf("replies = %q", replies)
	}
	if len(comments) == 0 || !strings.Contains(comments[0], "Opened PR #5.") {
		t.Fatalf("comments = %q, want the run's reply mirrored", comments)
	}
}
