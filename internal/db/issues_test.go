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
