package db

import (
	"context"
	"errors"
	"testing"
)

func TestAutoApprover(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	s.UpsertMember(ctx, "nkirsch", "admin", "test")
	s.UpsertMember(ctx, "boss2", "admin", "test")
	s.UpsertMember(ctx, "alice", "member", "test")
	src, _ := s.CreateItem(ctx, "Big ask", "", "alice")
	s.AddResponse(ctx, src, "nkirsch", "Also flag blocked items.", true)
	s.AddResponse(ctx, src, "alice", "+1", true)

	check := func(name, want string) {
		t.Helper()
		if got, err := s.AutoApprover(ctx, src); err != nil || got != want {
			t.Errorf("%s: got %q, %v; want %q", name, got, err, want)
		}
	}
	check("no one opted in", "")
	if err := s.SetAutoApprove(ctx, "NKirsch", true); err != nil {
		t.Fatal(err)
	}
	if m, _ := s.Member(ctx, "nkirsch"); !m.AutoApprove {
		t.Fatal("auto_approve not saved")
	}
	check("source unapproved", "")
	s.SetApproved(ctx, src, true, "boss2")
	check("admin commented", "nkirsch")
	s.SetAutoApprove(ctx, "boss2", true)
	check("only commenters count", "nkirsch")
	s.AddResponse(ctx, src, "boss2", "And split this.", true)
	check("latest admin post", "boss2")
	s.UpsertMember(ctx, "boss2", "member", "test")
	check("demoted admin", "nkirsch")
	if got, _ := s.AutoApprover(ctx, src+1); got != "" {
		t.Errorf("missing item: %q", got)
	}

	id, _ := s.CreateBotItem(ctx, "Split", "", "builder-bot")
	if err := s.ApproveBotItem(ctx, id, "nkirsch"); err != nil {
		t.Fatal(err)
	}
	it, _ := s.Item(ctx, id)
	if !it.Approved || it.ApprovedBy != "nkirsch" || it.HumanActivityAt.Year() > 1970 {
		t.Fatalf("bot item = %+v", it)
	}
	if err := s.SetAutoApprove(ctx, "nobody", true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing member: %v", err)
	}
}
