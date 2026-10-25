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

func TestItemsReplyCount(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	id, err := s.CreateItem(ctx, "Dark mode", "please", "alice")
	if err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{"+1", "me too"} {
		if _, err := s.AddResponse(ctx, id, "bob", body, true); err != nil {
			t.Fatal(err)
		}
	}
	items, err := s.Items(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].ID != id || items[0].Replies != 2 {
		t.Fatalf("items = %+v", items)
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
	s.ClaimRun(ctx, run, "", "")
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
	if set, err := s.UnreadItems(ctx, "ALICE"); err != nil || len(set) != 1 || !set[id] {
		t.Fatalf("unread items = %v, %v", set, err)
	}

	// Closed items leave the count but keep their marker on the Done board.
	s.CloseItem(ctx, id, "dup", true)
	if n, _ := s.UnreadCount(ctx, "alice"); n != 0 {
		t.Fatalf("unread after close = %d", n)
	}
	if set, _ := s.UnreadItems(ctx, "alice"); !set[id] {
		t.Fatalf("unread items after close = %v", set)
	}
	s.MarkRead(ctx, "alice", 0)
	if set, _ := s.UnreadItems(ctx, "alice"); len(set) != 0 {
		t.Fatalf("unread items after mark all = %v", set)
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
	if err := s.ClaimRun(ctx, run, "", ""); err != nil {
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
		s.ClaimRun(ctx, run, "", "")
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

func TestRunStages(t *testing.T) {
	t0 := time.Date(2026, 10, 25, 1, 0, 0, 0, time.UTC)
	at := func(s int) *time.Time { v := t0.Add(time.Duration(s) * time.Second); return &v }
	now := *at(400)
	type st struct {
		state string
		secs  int
	}
	for _, tc := range []struct {
		name string
		run  Run
		want []st
	}{
		{"requested", Run{CreatedAt: t0}, []st{{"active", 400}, {"pending", 0}, {"pending", 0}, {"pending", 0}}},
		{"agent", Run{CreatedAt: t0, StartedAt: at(10), AgentAt: at(40)}, []st{{"done", 10}, {"done", 30}, {"active", 360}, {"pending", 0}}},
		{"done", Run{CreatedAt: t0, StartedAt: at(10), AgentAt: at(40), ReportAt: at(300), FinishedAt: at(305)},
			[]st{{"done", 10}, {"done", 30}, {"done", 260}, {"done", 5}}},
		{"setup failed", Run{CreatedAt: t0, StartedAt: at(10), ReportAt: at(20), FinishedAt: at(21)},
			[]st{{"done", 10}, {"done", 10}, {"skipped", 0}, {"done", 1}}},
	} {
		got := tc.run.stagesAt(now)
		if len(got) != len(tc.want) {
			t.Fatalf("%s: %d stages", tc.name, len(got))
		}
		for i, w := range tc.want {
			if got[i].Name != RunStageNames[i] || got[i].State != w.state || got[i].Duration != time.Duration(w.secs)*time.Second {
				t.Errorf("%s: stage %d = %+v, want %+v", tc.name, i, got[i], w)
			}
		}
	}
	if got := (Run{CreatedAt: t0, StartedAt: at(10), FinishedAt: at(100)}).stagesAt(now); got != nil {
		t.Errorf("run without stage times: %+v", got)
	}
	if got := (Run{CreatedAt: t0, StartedAt: at(10), FinishedAt: at(100)}).runtimeAt(now); got != 90*time.Second {
		t.Errorf("finished runtime = %v", got)
	}
	if got := (Run{CreatedAt: t0, StartedAt: at(10)}).runtimeAt(now); got != 390*time.Second {
		t.Errorf("running runtime = %v", got)
	}
	if got := (Run{CreatedAt: t0}).runtimeAt(now); got != 0 {
		t.Errorf("requested runtime = %v", got)
	}
}

func TestSetRunStage(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	id, err := s.CreateRun(ctx, "manual", "nkirsch")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetRunStage(ctx, id, "agent"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("stage before claim: %v", err)
	}
	if err := s.ClaimRun(ctx, id, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.SetRunStage(ctx, id, "agent"); err != nil {
		t.Fatal(err)
	}
	r, _ := s.Run(ctx, id)
	first := *r.AgentAt
	if err := s.SetRunStage(ctx, id, "agent"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetRunStage(ctx, id, "report"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetRunStage(ctx, id, "agent_at = now(), status"); err == nil {
		t.Fatal("unknown stage accepted")
	}
	r, _ = s.Run(ctx, id)
	if !r.AgentAt.Equal(first) || r.ReportAt == nil || r.Progress() != 75 || r.Stage() != "report" {
		t.Fatalf("run = %+v, progress %d", r, r.Progress())
	}
	if err := s.FinishRun(ctx, id, "done", "", "", nil); err != nil {
		t.Fatal(err)
	}
	if err := s.SetRunStage(ctx, id, "report"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("stage after finish: %v", err)
	}
	r, _ = s.Run(ctx, id)
	if r.Progress() != 100 || r.Stage() != "" {
		t.Fatalf("finished progress %d stage %q", r.Progress(), r.Stage())
	}
}

func TestClaimRecordsRelease(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	id, err := s.CreateRun(ctx, "manual", "nkirsch")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ClaimRun(ctx, id, "abc1234def", "v26.10.25-3"); err != nil {
		t.Fatal(err)
	}
	r, err := s.Run(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if r.GitSHA != "abc1234def" || r.Version != "v26.10.25-3" {
		t.Fatalf("run = %+v", r)
	}
}

func TestRunStats(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	item, _ := s.CreateItem(ctx, "Stats", "", "alice")
	other, _ := s.CreateItem(ctx, "Other", "", "bob")
	s.SetApproved(ctx, item, true, "nkirsch")
	s.SetApproved(ctx, other, true, "nkirsch")

	newRun := func() int {
		id, err := s.CreateRun(ctx, "manual", "")
		if err != nil {
			t.Fatal(err)
		}
		s.ClaimRun(ctx, id, "", "")
		return id
	}
	r1 := newRun()
	if err := s.SetPR(ctx, item, 7); err != nil {
		t.Fatal(err)
	}
	if err := s.NoteRunPR(ctx, item, 7); err != nil {
		t.Fatal(err)
	}
	s.NoteRunPR(ctx, other, 8)
	s.SetPR(ctx, other, 8)
	s.MarkMerged(ctx, item, "abc")
	s.FinishRun(ctx, r1, "done", "", "", ptr(3.0))
	// 10s queued, 20s setup, 600s agent, 5s report = 625s runtime
	if _, err := s.Pool.Exec(ctx, `UPDATE runs SET created_at = $2::timestamptz, started_at = $2::timestamptz + interval '10 s',
		agent_at = $2::timestamptz + interval '30 s', report_at = $2::timestamptz + interval '630 s', finished_at = $2::timestamptz + interval '635 s' WHERE id = $1`,
		r1, time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	// an old failed run without stage times, outside the last 30 days
	r2 := newRun()
	s.FinishRun(ctx, r2, "failed", "", "", nil)
	if _, err := s.Pool.Exec(ctx, `UPDATE runs SET created_at = $2::timestamptz, started_at = $2::timestamptz, finished_at = $2::timestamptz + interval '25 s' WHERE id = $1`,
		r2, time.Now().Add(-40*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	newRun() // still running: not counted

	recent, err := s.RunStats(ctx, time.Now().Add(-30*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if recent.Runs != 1 || recent.Done != 1 || recent.Failed != 0 || recent.PRs != 2 || recent.PRsMerged != 1 ||
		recent.Runtime != 625*time.Second || recent.MedianRuntime != 625*time.Second || recent.CostPerPR == nil || *recent.CostPerPR != 1.5 {
		t.Fatalf("recent = %+v", recent)
	}
	for i, want := range []int{10, 20, 600, 5} {
		if recent.Stages[i].Name != RunStageNames[i] || recent.Stages[i].Duration != time.Duration(want)*time.Second {
			t.Errorf("stage %d = %+v", i, recent.Stages[i])
		}
	}
	all, err := s.RunStats(ctx, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if all.Runs != 2 || all.Failed != 1 || all.PRs != 2 || all.Runtime != 325*time.Second || all.Stages[2].Duration != 600*time.Second {
		t.Fatalf("all = %+v", all)
	}
}

func TestTakeRunToken(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	id, err := s.CreateRun(ctx, "manual", "nkirsch")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.TakeRunToken(ctx, id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("requested: %v", err)
	}
	s.ClaimRun(ctx, id, "", "")
	if err := s.TakeRunToken(ctx, id); err != nil {
		t.Fatalf("running: %v", err)
	}
	if err := s.TakeRunToken(ctx, id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second token: %v", err)
	}
	if err := s.TakeRunToken(ctx, id+1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing run: %v", err)
	}
}
