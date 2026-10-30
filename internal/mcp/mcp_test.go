package mcp

import (
	"context"
	"errors"
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

func TestDeployStatusDeployed(t *testing.T) {
	ctx := context.Background()
	b := testBoard(t)
	b.GitSHA = "hub"
	if _, out, err := b.deployStatus(ctx, nil, struct{}{}); err != nil || out.GitSHA != "hub" {
		t.Fatalf("own sha: %+v %v", out, err)
	}
	b.Deployed = func(context.Context) (string, error) { return "app", nil }
	if _, out, err := b.deployStatus(ctx, nil, struct{}{}); err != nil || out.GitSHA != "app" {
		t.Fatalf("app sha: %+v %v", out, err)
	}
	b.Deployed = func(context.Context) (string, error) { return "", errors.New("down") }
	if _, out, err := b.deployStatus(ctx, nil, struct{}{}); err != nil || out.GitSHA != "" || out.CheckedAtNote != "can't read the deployed SHA: down" {
		t.Fatalf("unreadable: %+v %v", out, err)
	}
}

// The agent can't tag an item urgent: that tag's merge cuts a release.
func TestSetTagsRefusesUrgent(t *testing.T) {
	ctx := context.Background()
	b := testBoard(t)
	id, _ := b.Store.CreateItem(ctx, "Fix", "", "alice")
	if _, _, err := b.setTags(ctx, nil, SetTagsIn{ID: id, Tags: []string{"bug", "#Urgent"}}); err == nil {
		t.Fatal("urgent tag accepted")
	}
	if it, _ := b.Store.Item(ctx, id); len(it.Tags) != 0 {
		t.Fatalf("tags = %v", it.Tags)
	}
	if _, _, err := b.setTags(ctx, nil, SetTagsIn{ID: id, Tags: []string{"bug"}}); err != nil {
		t.Fatal(err)
	}
}
