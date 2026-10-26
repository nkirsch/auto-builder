package db

import (
	"context"
	"slices"
	"testing"
)

func TestEscalate(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	id, _ := s.CreateItem(ctx, "Site down", "", "alice")
	other, _ := s.CreateItem(ctx, "Typo", "", "alice")
	before, _ := s.Item(ctx, id)

	if err := s.Escalate(ctx, id, "admin", "alice", []string{"nkirsch", "k@example.org"}); err != nil {
		t.Fatal(err)
	}
	it, _ := s.Item(ctx, id)
	if it.Escalation != "admin" || it.EscalatedBy != "alice" || it.EscalatedAt == nil ||
		!slices.Equal(it.Assignees, []string{"nkirsch", "k@example.org"}) || !it.HumanActivityAt.After(before.HumanActivityAt) {
		t.Fatalf("escalated item = %+v", it)
	}
	if err := s.Escalate(ctx, id, "platform", "nkirsch", []string{"root"}); err != nil {
		t.Fatal(err)
	}
	s.Escalate(ctx, other, "platform", "nkirsch", nil)
	s.CloseItem(ctx, other, "dup", true)
	if got, err := s.Escalated(ctx, "platform"); err != nil || len(got) != 1 || got[0].ID != id {
		t.Fatalf("Escalated(platform) = %+v, %v; want only the open item", got, err)
	}
	if got, _ := s.Escalated(ctx, "admin"); len(got) != 0 {
		t.Fatalf("Escalated(admin) = %+v", got)
	}

	if err := s.Escalate(ctx, id, "", "nkirsch", []string{"root"}); err != nil {
		t.Fatal(err)
	}
	if it, _ = s.Item(ctx, id); it.Escalation != "" || it.EscalatedBy != "" || it.EscalatedAt != nil || len(it.Assignees) != 0 {
		t.Fatalf("cleared item = %+v", it)
	}
	if err := s.Escalate(ctx, 999, "admin", "alice", nil); err != ErrNotFound {
		t.Fatalf("missing item: %v", err)
	}
}
