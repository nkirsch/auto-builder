package gate

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/nkirsch/auto-builder/internal/db"
	"github.com/nkirsch/auto-builder/internal/github"
)

// TestShipSoon needs TEST_DATABASE_URL (a disposable database; tables are truncated).
func TestShipSoon(t *testing.T) {
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

	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/o/r/pulls/5":
			w.Write([]byte(`{"number":5,"state":"closed","merged":true,"merge_commit_sha":"abcdef123456"}`))
		case "/repos/o/r/compare/deployed...abcdef123456":
			w.Write([]byte(`{"status":"behind"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer gh.Close()

	var mu sync.Mutex
	var replies []string
	g := &Gate{Store: st, Ship: &Shipper{
		Store: st, GitHub: github.New(gh.URL, ""), Repo: "o/r", GitSHA: "deployed",
		Reply: func(_ context.Context, _ db.Item, body string) error {
			mu.Lock()
			defer mu.Unlock()
			replies = append(replies, body)
			return nil
		},
	}}
	g.ShipSoon()

	deadline := time.Now().Add(5 * time.Second)
	for {
		it, err := st.Item(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if it.Status == "shipped" {
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
}
