package gate

import (
	"testing"
	"time"

	"github.com/nkirsch/auto-builder/internal/db"
)

func TestDecide(t *testing.T) {
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	ago := func(d time.Duration) *time.Time { t := now.Add(-d); return &t }
	idle := 6 * time.Hour
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
	}
	for _, c := range cases {
		if got, reason := Decide(c.s, now, c.trigger, idle); got != c.want {
			t.Errorf("%s: got %v (%s), want %v", c.name, got, reason, c.want)
		}
	}
}
