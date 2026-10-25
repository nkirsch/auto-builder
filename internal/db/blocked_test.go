package db

import (
	"context"
	"errors"
	"testing"
)

func TestBlocked(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	id, _ := s.CreateItem(ctx, "Needs bootstrap", "", "alice")
	s.SetApproved(ctx, id, true, "nkirsch")
	run, _ := s.CreateRun(ctx, "tick", "")
	s.ClaimRun(ctx, run, "", "")
	s.FinishRun(ctx, run, "done", "", "", nil)
	before, _ := s.Item(ctx, id)

	if err := s.SetBlocked(ctx, id, "Re-run infra/bootstrap.sh.", false); err != nil {
		t.Fatal(err)
	}
	it, _ := s.Item(ctx, id)
	if it.Blocked != "Re-run infra/bootstrap.sh." || it.BlockedAt == nil || !it.HumanActivityAt.Equal(before.HumanActivityAt) {
		t.Fatalf("blocked item = %+v", it)
	}
	g, _ := s.GateState(ctx)
	if g.ApprovedOpen || g.NewActivity {
		t.Fatalf("gate counts blocked item: %+v", g)
	}
	// A human reply lets the gate see it again, though it stays flagged.
	if _, err := s.AddResponse(ctx, id, "nkirsch", "Done, re-ran it.", true); err != nil {
		t.Fatal(err)
	}
	if g, _ = s.GateState(ctx); !g.ApprovedOpen {
		t.Fatalf("gate ignores blocked item after human reply: %+v", g)
	}
	if err := s.SetBlocked(ctx, id, "", true); err != nil {
		t.Fatal(err)
	}
	if it, _ = s.Item(ctx, id); it.Blocked != "" || it.BlockedAt != nil {
		t.Fatalf("cleared item = %+v", it)
	}

	s.SetBlocked(ctx, id, "CI failed on PR #3.", false)
	s.SetPR(ctx, id, 3)
	s.MarkMerged(ctx, id, "abc")
	if it, _ = s.Item(ctx, id); it.Blocked != "" {
		t.Fatalf("merge left item blocked: %q", it.Blocked)
	}
	if err := s.SetBlocked(ctx, id+1, "x", false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing item: %v", err)
	}
}
