package db

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestImportIssue(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	id, created, err := s.ImportIssue(ctx, 7, "Crash on login", "stack trace", "alice")
	if err != nil || !created {
		t.Fatalf("import: id=%d created=%v err=%v", id, created, err)
	}
	if _, created, err := s.ImportIssue(ctx, 7, "Crash on login", "again", "alice"); err != nil || created {
		t.Fatalf("re-import: created=%v err=%v", created, err)
	}
	it, err := s.ItemByIssue(ctx, 7)
	if err != nil || it.ID != id || it.IssueNumber == nil || *it.IssueNumber != 7 || it.Approved || it.Status != "open" {
		t.Fatalf("item = %+v, err = %v", it, err)
	}
	if _, err := s.ItemByIssue(ctx, 8); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing issue: %v", err)
	}

	before := it.HumanActivityAt
	time.Sleep(10 * time.Millisecond)
	if created, err := s.ImportIssueComment(ctx, id, 900, "bob", "same here"); err != nil || !created {
		t.Fatalf("comment: created=%v err=%v", created, err)
	}
	if created, err := s.ImportIssueComment(ctx, id, 900, "bob", "same here"); err != nil || created {
		t.Fatalf("re-import comment: created=%v err=%v", created, err)
	}
	if rs, _ := s.Responses(ctx, id); len(rs) != 1 || rs[0].Author != "bob" {
		t.Fatalf("responses = %+v", rs)
	}
	if it, _ = s.Item(ctx, id); !it.HumanActivityAt.After(before) {
		t.Fatal("imported comment isn't human activity")
	}

	if todo, _ := s.IssuesToClose(ctx); len(todo) != 0 {
		t.Fatalf("open item's issue to close: %+v", todo)
	}
	s.CloseItem(ctx, id, "dup", false)
	plain, _ := s.CreateItem(ctx, "No issue", "", "alice")
	s.CloseItem(ctx, plain, "dup", false)
	if todo, _ := s.IssuesToClose(ctx); len(todo) != 1 || todo[0].ID != id {
		t.Fatalf("issues to close = %+v", todo)
	}
	if err := s.MarkIssueClosed(ctx, id); err != nil {
		t.Fatal(err)
	}
	if todo, _ := s.IssuesToClose(ctx); len(todo) != 0 {
		t.Fatalf("closed issue still to close: %+v", todo)
	}

	if at, err := s.IssuesSyncedAt(ctx); err != nil || at != nil {
		t.Fatalf("synced at = %v, %v", at, err)
	}
	now := time.Now().Truncate(time.Microsecond)
	s.SetIssuesSyncedAt(ctx, now)
	if at, _ := s.IssuesSyncedAt(ctx); at == nil || !at.Equal(now) {
		t.Fatalf("synced at = %v, want %v", at, now)
	}
}

func TestMirrorQueries(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	pub, _ := s.CreateItem(ctx, "Public", "", "alice")
	priv, _ := s.CreateItem(ctx, "Secret", "#private", "alice")
	s.SetTags(ctx, priv, []string{"private"}, false)
	done, _ := s.CreateItem(ctx, "Done", "", "alice")
	s.CloseItem(ctx, done, "dup", false)
	fresh, _ := s.CreateItem(ctx, "Just filed", "", "alice")
	s.Pool.Exec(ctx, `UPDATE items SET created_at = now() - interval '2 minutes' WHERE id <> $1`, fresh)

	if todo, err := s.ItemsToMirror(ctx); err != nil || len(todo) != 1 || todo[0].ID != pub {
		t.Fatalf("to mirror = %+v, %v", todo, err)
	}
	if err := s.SetItemIssue(ctx, pub, 40); err != nil {
		t.Fatal(err)
	}
	if err := s.SetItemIssue(ctx, pub, 41); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second issue: %v", err)
	}
	if todo, _ := s.ItemsToMirror(ctx); len(todo) != 0 {
		t.Fatalf("mirrored item still to mirror: %+v", todo)
	}

	r1, _ := s.AddResponse(ctx, pub, "bob", "hi", true)
	s.AddResponse(ctx, pub, "bob", "  ", true)
	s.AddResponse(ctx, priv, "bob", "hidden", true)
	s.ImportIssueComment(ctx, pub, 500, "bob", "from GitHub")
	rs, err := s.RepliesToMirror(ctx)
	if err != nil || len(rs) != 1 || rs[0].ID != r1 || rs[0].Issue != 40 || rs[0].Body != "hi" {
		t.Fatalf("replies to mirror = %+v, %v", rs, err)
	}
	if err := s.SetReplyIssueComment(ctx, r1, 501); err != nil {
		t.Fatal(err)
	}
	if rs, _ := s.RepliesToMirror(ctx); len(rs) != 0 {
		t.Fatalf("mirrored reply still to mirror: %+v", rs)
	}

	s.CloseItem(ctx, pub, "dup", false)
	s.MarkIssueClosed(ctx, pub)
	if todo, _ := s.IssuesToReopen(ctx); len(todo) != 0 {
		t.Fatalf("closed item's issue to reopen: %+v", todo)
	}
	s.ReopenItem(ctx, pub)
	if todo, _ := s.IssuesToReopen(ctx); len(todo) != 1 || todo[0].ID != pub {
		t.Fatalf("issues to reopen = %+v", todo)
	}
	if err := s.MarkIssueReopened(ctx, pub); err != nil {
		t.Fatal(err)
	}
	if todo, _ := s.IssuesToReopen(ctx); len(todo) != 0 {
		t.Fatalf("reopened issue still to reopen: %+v", todo)
	}
}

// An item takes its issue's number when that's free, and the sequence moves past it.
func TestCreateIssueItemNumbers(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	if id, _, err := s.CreateIssueItem(ctx, 12, "From GitHub", "", "alice", false); err != nil || id != 12 {
		t.Fatalf("issue 12: id=%d err=%v", id, err)
	}
	if id, err := s.CreateItem(ctx, "Unsynced", "", "alice"); err != nil || id != 13 {
		t.Fatalf("sequence after a numbered item: id=%d err=%v", id, err)
	}
	// Issue 13's number is taken on the board: it gets the next free one.
	id, created, err := s.CreateIssueItem(ctx, 13, "Raced", "", "builder-bot", true)
	if err != nil || !created || id != 14 {
		t.Fatalf("issue 13: id=%d created=%v err=%v", id, created, err)
	}
	if it, _ := s.Item(ctx, id); it.IssueNumber == nil || *it.IssueNumber != 13 || it.HumanActivityAt.Unix() != 0 {
		t.Fatalf("raced item %+v", it)
	}
	if _, created, err := s.CreateIssueItem(ctx, 13, "Again", "", "alice", false); err != nil || created {
		t.Fatalf("issue 13 twice: created=%v err=%v", created, err)
	}
}
