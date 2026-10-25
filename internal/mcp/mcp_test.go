package mcp

import (
	"context"
	"os"
	"testing"

	"github.com/nkirsch/auto-builder/internal/db"
)

// testBoard needs TEST_DATABASE_URL (a disposable database; tables are truncated).
func testBoard(t *testing.T) *Board {
	t.Helper()
	u := os.Getenv("TEST_DATABASE_URL")
	if u == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	pool, err := db.Open(ctx, u)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(ctx, `TRUNCATE members, items, responses, attachments, notifications, runs RESTART IDENTITY CASCADE`); err != nil {
		t.Fatal(err)
	}
	return &Board{Store: &db.Store{Pool: pool}, BotLogin: "builder-bot"}
}

func TestItemOutBlockedAt(t *testing.T) {
	ctx := context.Background()
	b := testBoard(t)
	id, _ := b.Store.CreateItem(ctx, "Needs bootstrap", "", "alice")
	it, _ := b.Store.Item(ctx, id)
	if out, err := b.itemOut(ctx, it, nil, false); err != nil || out.Blocked != "" || out.BlockedAt != nil {
		t.Fatalf("unblocked out = %+v, %v", out, err)
	}
	if err := b.Store.SetBlocked(ctx, id, "Re-run infra/bootstrap.sh.", false); err != nil {
		t.Fatal(err)
	}
	it, _ = b.Store.Item(ctx, id)
	out, err := b.itemOut(ctx, it, nil, false)
	if err != nil || out.Blocked != "Re-run infra/bootstrap.sh." || out.BlockedAt == nil || !out.BlockedAt.Equal(*it.BlockedAt) {
		t.Fatalf("blocked out = %+v, %v", out, err)
	}
}
