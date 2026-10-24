package main

import (
	"strings"
	"testing"
)

func TestSummaryPrefersResult(t *testing.T) {
	log := "$ claude\n" + `{"type":"assistant","message":{}}` + "\n" + `{"type":"result","result":"Opened PR #4 for item 2."}` + "\n"
	if got := summary([]byte(log)); got != "Opened PR #4 for item 2." {
		t.Fatalf("got %q", got)
	}
}

func TestSummaryTail(t *testing.T) {
	log := strings.Repeat("x", 5000) + "END"
	got := summary([]byte(log))
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
