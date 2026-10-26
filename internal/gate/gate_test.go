package gate

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/nkirsch/auto-builder/internal/db"
)

func TestDecide(t *testing.T) {
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	ago := func(d time.Duration) *time.Time { t := now.Add(-d); return &t }
	idle := 6 * time.Hour
	// Cases that leave SpendLimit unset run under a $20 limit with nothing spent.
	under := func(s db.GateState) db.GateState {
		if s.SpendLimit == 0 {
			s.SpendLimit = 20
		}
		return s
	}
	cases := []struct {
		name    string
		s       db.GateState
		trigger string
		want    bool
	}{
		{"active blocks now", db.GateState{Active: true}, "now", false},
		{"active blocks activity", db.GateState{Active: true, NewActivity: true}, "activity", false},
		{"now overrides failure backoff", db.GateState{LastFailure: ago(time.Minute)}, "now", true},
		{"activity launches", db.GateState{NewActivity: true, LastSuccess: ago(time.Hour)}, "activity", true},
		{"recent failure holds activity", db.GateState{NewActivity: true, LastFailure: ago(10 * time.Minute)}, "activity", false},
		{"old failure lets activity through", db.GateState{NewActivity: true, LastFailure: ago(31 * time.Minute)}, "tick", true},
		{"idle with approved work", db.GateState{ApprovedOpen: true, LastSuccess: ago(7 * time.Hour)}, "tick", true},
		{"not idle long enough", db.GateState{ApprovedOpen: true, LastSuccess: ago(5 * time.Hour)}, "tick", false},
		{"idle but nothing approved", db.GateState{LastSuccess: ago(7 * time.Hour)}, "tick", false},
		{"never run, approved work", db.GateState{ApprovedOpen: true}, "tick", true},
		{"quiet", db.GateState{LastSuccess: ago(time.Hour)}, "tick", false},
		{"limit reached blocks now", db.GateState{Spend30: 20, SpendLimit: 20}, "now", false},
		{"limit reached blocks activity", db.GateState{NewActivity: true, Spend30: 25, SpendLimit: 20}, "activity", false},
		{"under limit launches", db.GateState{NewActivity: true, Spend30: 19.99, SpendLimit: 20}, "activity", true},
	}
	for _, c := range cases {
		if got, reason := Decide(under(c.s), now, c.trigger, idle); got != c.want {
			t.Errorf("%s: got %v (%s), want %v", c.name, got, reason, c.want)
		}
	}
}

// fakeLauncher launches as "exec-<id>" and records cancels.
type fakeLauncher struct{ cancelled []string }

func (f *fakeLauncher) Launch(_ context.Context, id int) (string, error) {
	return fmt.Sprintf("exec-%d", id), nil
}

func (f *fakeLauncher) Cancel(_ context.Context, _ int, execution string) error {
	f.cancelled = append(f.cancelled, execution)
	return nil
}

func TestCancel(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	l := &fakeLauncher{}
	g := &Gate{Store: st, Launcher: l}
	id, err := g.RunNow(ctx, "nkirsch")
	if err != nil {
		t.Fatal(err)
	}
	if err := g.Cancel(ctx, id, "nkirsch"); err != nil {
		t.Fatal(err)
	}
	r, _ := st.Run(ctx, id)
	if r.Status != "cancelled" || r.Summary != "Cancelled by nkirsch." || r.FinishedAt == nil {
		t.Errorf("run %+v", r)
	}
	if len(l.cancelled) != 1 || l.cancelled[0] != fmt.Sprintf("exec-%d", id) {
		t.Errorf("cancelled %v", l.cancelled)
	}
	if err := g.Cancel(ctx, id, "nkirsch"); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("cancel twice: %v", err)
	}
	// A cancelled run isn't a failure: no backoff, and the next run can start.
	if s, _ := st.GateState(ctx); s.LastFailure != nil || s.Active {
		t.Errorf("gate state %+v", s)
	}
	if _, err := g.RunNow(ctx, "nkirsch"); err != nil {
		t.Errorf("run after cancel: %v", err)
	}
}
