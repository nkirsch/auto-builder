package main

import (
	"strings"
	"testing"
)

func TestSummaryPrefersResult(t *testing.T) {
	log := "$ claude\n" + `{"type":"assistant","message":{}}` + "\n" +
		`{"type":"result","result":"Opened PR #4 for item 2.","total_cost_usd":1.2345}` + "\n"
	got, cost := result([]byte(log))
	if got != "Opened PR #4 for item 2." {
		t.Fatalf("got %q", got)
	}
	if cost == nil || *cost != 1.2345 {
		t.Fatalf("cost = %v", cost)
	}
}

func TestSummaryTail(t *testing.T) {
	log := strings.Repeat("x", 5000) + "END"
	got, cost := result([]byte(log))
	if cost != nil {
		t.Fatalf("cost = %v, want nil", *cost)
	}
	if len(got) != summaryLen || !strings.HasSuffix(got, "END") {
		t.Fatalf("len %d, suffix %q", len(got), got[len(got)-3:])
	}
}

func TestCapped(t *testing.T) {
	c := &capped{max: 5}
	c.Write([]byte("abc"))
	c.Write([]byte("defg"))
	if c.String() != "abcde" {
		t.Fatalf("got %q", c.String())
	}
}
