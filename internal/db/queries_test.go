package db

import (
	"context"
	"errors"
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
	if _, err := pool.Exec(ctx, `TRUNCATE members, items, responses, attachments, notifications, runs RESTART IDENTITY CASCADE`); err != nil {
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
	if err := s.FinishRun(ctx, run, "done", "ok", ""); err != nil {
		t.Fatal(err)
	}
	g, _ = s.GateState(ctx)
	if g.Active || g.NewActivity || !g.ApprovedOpen || g.LastSuccess == nil {
		t.Fatalf("after run: %+v", g)
	}
	if err := s.FinishRun(ctx, run, "failed", "", ""); !errors.Is(err, ErrNotFound) {
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
	s.Notify(ctx, "alice", id, "bob", "mention")
	if n, _ := s.UnreadCount(ctx, "ALICE"); n != 2 {
		t.Fatalf("unread = %d", n)
	}
	if err := s.MarkRead(ctx, "alice", id); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.UnreadCount(ctx, "alice"); n != 0 {
		t.Fatalf("unread after read = %d", n)
	}
	ns, err := s.Notifications(ctx, "alice", false, 10)
	if err != nil || len(ns) != 2 || ns[0].ItemTitle != "x" {
		t.Fatalf("notifications = %+v, %v", ns, err)
	}

	// Closing the item moves read notifications to done; unread ones stay in the inbox.
	s.CloseItem(ctx, id, "dup", true)
	s.Notify(ctx, "alice", id, "bob", "reply")
	inbox, _ := s.Notifications(ctx, "alice", false, 10)
	done, _ := s.Notifications(ctx, "alice", true, 10)
	if len(inbox) != 1 || inbox[0].ReadAt != nil || len(done) != 2 {
		t.Fatalf("inbox = %+v, done = %+v", inbox, done)
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
