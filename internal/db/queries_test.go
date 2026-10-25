package db

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	ctx := context.Background()
	pool, err := Open(ctx, testURL(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(ctx, `TRUNCATE members, items, responses, attachments, notifications, runs, run_items RESTART IDENTITY CASCADE;
		UPDATE settings SET spend_limit_usd = 20`); err != nil {
		t.Fatal(err)
	}
	return &Store{Pool: pool}
}

func TestItemLifecycle(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	id, err := s.CreateItem(ctx, "Dark mode", "please", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetApproved(ctx, id, true, "nkirsch"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetPR(ctx, id, 7); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkShipped(ctx, id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("shipped straight from building: %v", err)
	}
	if err := s.MarkMerged(ctx, id, "abc123"); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkShipped(ctx, id); err != nil {
		t.Fatal(err)
	}
	it, err := s.Item(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if it.Status != "shipped" || it.MergeSHA != "abc123" || *it.PRNumber != 7 || it.ApprovedBy != "nkirsch" {
		t.Fatalf("item = %+v", it)
	}
	if _, err := s.Item(ctx, 999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing item: %v", err)
	}
}

func TestBotActivityDoesNotCount(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	id, _ := s.CreateItem(ctx, "x", "", "alice")
	before, _ := s.Item(ctx, id)
	time.Sleep(10 * time.Millisecond)
	if _, err := s.AddResponse(ctx, id, "builder-bot", "question?", false); err != nil {
		t.Fatal(err)
	}
	after, _ := s.Item(ctx, id)
	if !after.HumanActivityAt.Equal(before.HumanActivityAt) {
		t.Fatal("bot reply bumped human_activity_at")
	}
	if _, err := s.AddResponse(ctx, id, "alice", "answer", true); err != nil {
		t.Fatal(err)
	}
	human, _ := s.Item(ctx, id)
	if !human.HumanActivityAt.After(before.HumanActivityAt) {
		t.Fatal("human reply did not bump human_activity_at")
	}
	if _, err := s.AddResponse(ctx, 999, "alice", "x", true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("reply to missing item: %v", err)
	}
}

func TestGateState(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	g, err := s.GateState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if g.Active || g.NewActivity || g.ApprovedOpen || g.LastSuccess != nil {
		t.Fatalf("empty board: %+v", g)
	}

	id, _ := s.CreateItem(ctx, "x", "", "alice")
	if g, _ = s.GateState(ctx); !g.NewActivity || g.ApprovedOpen {
		t.Fatalf("new item: %+v", g)
	}
	s.SetApproved(ctx, id, true, "nkirsch")

	run, err := s.CreateRun(ctx, "activity", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateRun(ctx, "now", "nkirsch"); !errors.Is(err, ErrRunActive) {
		t.Fatalf("second run: %v", err)
	}
	s.ClaimRun(ctx, run)
	if err := s.FinishRun(ctx, run, "done", "ok", "", nil); err != nil {
		t.Fatal(err)
	}
	g, _ = s.GateState(ctx)
	if g.Active || g.NewActivity || !g.ApprovedOpen || g.LastSuccess == nil {
		t.Fatalf("after run: %+v", g)
	}
	if err := s.FinishRun(ctx, run, "failed", "", "", nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("finished twice: %v", err)
	}

	time.Sleep(10 * time.Millisecond)
	s.AddResponse(ctx, id, "alice", "more", true)
	if g, _ = s.GateState(ctx); !g.NewActivity {
		t.Fatalf("reply after run: %+v", g)
	}
}

func TestFailStale(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	run, _ := s.CreateRun(ctx, "tick", "")
	s.Pool.Exec(ctx, `UPDATE runs SET created_at = now() - interval '1 hour' WHERE id = $1`, run)
	n, err := s.FailStale(ctx, 70*time.Minute, 15*time.Minute)
	if err != nil || n != 1 {
		t.Fatalf("FailStale = %d, %v", n, err)
	}
	r, _ := s.Run(ctx, run)
	if r.Status != "failed" || r.Summary != "never started" {
		t.Fatalf("run = %+v", r)
	}
}

func TestNotifications(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	id, _ := s.CreateItem(ctx, "x", "", "alice")
	s.Notify(ctx, "Alice", id, "bob", "reply")
	s.Notify(ctx, "alice", id, "carol", "mention")
	other, _ := s.CreateItem(ctx, "y", "", "alice")
	s.Notify(ctx, "alice", other, "bob", "reply")
	if n, _ := s.UnreadCount(ctx, "ALICE"); n != 2 {
		t.Fatalf("unread = %d", n)
	}
	if err := s.MarkRead(ctx, "alice", other); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.UnreadCount(ctx, "alice"); n != 1 {
		t.Fatalf("unread after read = %d", n)
	}
	// One row per item: its latest notification, newest item first.
	ns, err := s.Notifications(ctx, "alice", false, 10)
	if err != nil || len(ns) != 2 || ns[0].ItemID != other || ns[0].ReadAt == nil ||
		ns[1].ItemID != id || ns[1].Actor != "carol" || ns[1].Count != 2 || ns[1].ReadAt != nil {
		t.Fatalf("notifications = %+v, %v", ns, err)
	}

	// Closing an item moves it to done right away, unread or not.
	s.CloseItem(ctx, id, "dup", true)
	inbox, _ := s.Notifications(ctx, "alice", false, 10)
	done, _ := s.Notifications(ctx, "alice", true, 10)
	if len(inbox) != 1 || inbox[0].ItemID != other || len(done) != 1 || done[0].ItemID != id || done[0].ReadAt != nil {
		t.Fatalf("inbox = %+v, done = %+v", inbox, done)
	}
	if n, _ := s.UnreadCount(ctx, "alice"); n != 0 {
		t.Fatalf("unread after close = %d", n)
	}
	s.CloseItem(ctx, other, "dup", true)
	inbox, _ = s.Notifications(ctx, "alice", false, 10)
	done, _ = s.Notifications(ctx, "alice", true, 10)
	if len(inbox) != 0 || len(done) != 2 {
		t.Fatalf("after close: inbox = %+v, done = %+v", inbox, done)
	}
}

func TestMembersCaseInsensitive(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	s.UpsertMember(ctx, "NKirsch", "admin", "bootstrap")
	m, err := s.Member(ctx, "nkirsch")
	if err != nil || !m.IsAdmin() {
		t.Fatalf("member = %+v, %v", m, err)
	}
	if _, err := s.Member(ctx, "nobody"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing member: %v", err)
	}
}

func TestRunItems(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	a, _ := s.CreateItem(ctx, "Tags", "", "alice")
	b, _ := s.CreateItem(ctx, "Dupes", "", "alice")
	if err := s.NoteRunItem(ctx, a); err != nil {
		t.Fatal(err)
	}
	run, err := s.CreateRun(ctx, "tick", "")
	if err != nil {
		t.Fatal(err)
	}
	s.NoteRunItem(ctx, a) // requested, not yet running
	if err := s.ClaimRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	for _, id := range []int{b, a, b} {
		if err := s.NoteRunItem(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	c, _ := s.CreateItem(ctx, "Costs", "", "alice")
	summary := fmt.Sprintf("Built nothing.\n- **#%d** (open): skipped\n- **#%d**: replied\n- PR #9999 merged", c, a)
	if err := s.FinishRun(ctx, run, "done", summary, "", nil); err != nil {
		t.Fatal(err)
	}
	s.NoteRunItem(ctx, a) // after the run
	got, err := s.RunItems(ctx, []int{run, 99})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || len(got[run]) != 3 || got[run][0].ItemID != b || got[run][1].ItemID != a ||
		got[run][2].ItemID != c || got[run][0].Title != "Dupes" || got[run][1].Status != "open" {
		t.Fatalf("run items = %+v", got)
	}
}

func TestSummaryItems(t *testing.T) {
	got := SummaryItems("Nothing built (#4 was skipped).\n\n- **#10** (building): left\n  * #13: tagged\n#11 skipped\n- **#10** again\n- PR #16 merged\n#abc")
	if !slices.Equal(got, []int{10, 13, 11}) {
		t.Fatalf("SummaryItems = %v", got)
	}
}

func TestBotItemReadable(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	id, err := s.CreateBotItem(ctx, "split out", "", "builder-bot")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Item(ctx, id); err != nil {
		t.Fatalf("Item: %v", err)
	}
	if items, err := s.Items(ctx); err != nil || len(items) != 1 {
		t.Fatalf("Items = %d, %v", len(items), err)
	}
	if g, _ := s.GateState(ctx); g.NewActivity {
		t.Fatal("bot-filed item counts as human activity")
	}
	// Rows written before the fix are repaired by migration 003.
	s.Pool.Exec(ctx, `UPDATE items SET human_activity_at = '-infinity' WHERE id = $1`, id)
	s.Pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version = '003_bot_item_activity'`)
	if err := Migrate(ctx, s.Pool); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Items(ctx); err != nil {
		t.Fatalf("Items after migration: %v", err)
	}
}

func TestSetTags(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	id, _ := s.CreateItem(ctx, "x", "", "alice")
	before, _ := s.Item(ctx, id)
	if len(before.Tags) != 0 {
		t.Fatalf("new item tags = %v", before.Tags)
	}
	if err := s.SetTags(ctx, id, []string{"bug"}, true); err != nil {
		t.Fatal(err)
	}
	if err := s.SetTags(ctx, id, []string{"feature"}, true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("retagged a tagged item: %v", err)
	}
	it, _ := s.Item(ctx, id)
	if len(it.Tags) != 1 || it.Tags[0] != "bug" || !it.HumanActivityAt.Equal(before.HumanActivityAt) {
		t.Fatalf("item = %+v", it)
	}
	items, _ := s.Items(ctx, "open")
	if len(items) != 1 || items[0].Tags[0] != "bug" {
		t.Fatalf("items = %+v", items)
	}
}

func TestRunCosts(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	for _, cost := range []*float64{ptr(1.25), nil, ptr(0.5)} {
		run, err := s.CreateRun(ctx, "tick", "")
		if err != nil {
			t.Fatal(err)
		}
		if err := s.FinishRun(ctx, run, "done", "", "", cost); err != nil {
			t.Fatal(err)
		}
		r, _ := s.Run(ctx, run)
		if (r.CostUSD == nil) != (cost == nil) || (cost != nil && *r.CostUSD != *cost) {
			t.Fatalf("run %d cost = %v, want %v", run, r.CostUSD, cost)
		}
	}
	s.Pool.Exec(ctx, `UPDATE runs SET created_at = now() - interval '40 days' WHERE cost_usd = 1.25`)
	c, err := s.RunCosts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if c != (RunCosts{Total: 1.75, Last30Days: 0.5, Runs: 2}) {
		t.Fatalf("RunCosts = %+v", c)
	}
}

func TestSpendLimit(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	if v, err := s.SpendLimit(ctx); err != nil || v != 20 {
		t.Fatalf("default limit = %v, %v; want 20", v, err)
	}
	if err := s.SetSpendLimit(ctx, 35.5); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSpendLimit(ctx, 0); err == nil {
		t.Error("SetSpendLimit(0) succeeded")
	}
	for _, cost := range []float64{4, 6} {
		run, _ := s.CreateRun(ctx, "tick", "")
		s.FinishRun(ctx, run, "done", "", "", &cost)
	}
	s.Pool.Exec(ctx, `UPDATE runs SET created_at = now() - interval '31 days' WHERE cost_usd = 4`)
	g, err := s.GateState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if g.Spend30 != 6 || g.SpendLimit != 35.5 {
		t.Fatalf("GateState spend = %v of %v, want 6 of 35.5", g.Spend30, g.SpendLimit)
	}
}

func TestMemberCosts(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	a, _ := s.CreateItem(ctx, "A", "", "alice")
	b, _ := s.CreateItem(ctx, "B", "", "bob")
	finish := func(cost *float64, items ...int) int {
		t.Helper()
		run, err := s.CreateRun(ctx, "tick", "")
		if err != nil {
			t.Fatal(err)
		}
		s.ClaimRun(ctx, run)
		for _, id := range items {
			s.NoteRunItem(ctx, id)
		}
		if err := s.FinishRun(ctx, run, "done", "", "", cost); err != nil {
			t.Fatal(err)
		}
		return run
	}
	finish(ptr(3.0), a, b)     // 1.5 each
	old := finish(ptr(2.0), a) // alice, 40 days ago
	finish(ptr(9.0))           // no items: unattributed
	finish(nil, b)             // no cost reported
	s.Pool.Exec(ctx, `UPDATE runs SET created_at = now() - interval '40 days' WHERE id = $1`, old)
	got, err := s.MemberCosts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := []MemberCost{{"alice", 3.5, 1.5}, {"bob", 1.5, 1.5}}
	if !slices.Equal(got, want) {
		t.Fatalf("MemberCosts = %+v, want %+v", got, want)
	}
}

func ptr[T any](v T) *T { return &v }
