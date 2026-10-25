package board

import (
	"testing"
	"time"
)

func TestNextTag(t *testing.T) {
	for _, c := range []struct {
		existing []string
		want     string
	}{
		{nil, "v26.10.24"},
		{[]string{"v26.10.24"}, "v26.10.24-1"},
		{[]string{"v26.10.24", "v26.10.24-1", "v26.10.24-3"}, "v26.10.24-4"},
		{[]string{"v26.10.240"}, "v26.10.24"}, // another day's prefix match
	} {
		if got := nextTag("v26.10.24", c.existing); got != c.want {
			t.Errorf("nextTag(%v) = %s, want %s", c.existing, got, c.want)
		}
	}
}

func TestReleaseDayPacific(t *testing.T) {
	// 2026-10-25 03:00 UTC is still Oct 24 in Pacific time.
	if got := releaseDay(time.Date(2026, 10, 25, 3, 0, 0, 0, time.UTC)); got != "v26.10.24" {
		t.Errorf("releaseDay = %s, want v26.10.24", got)
	}
	if got := releaseDay(time.Date(2026, 3, 5, 20, 0, 0, 0, time.UTC)); got != "v26.3.5" {
		t.Errorf("releaseDay = %s, want v26.3.5", got)
	}
}
