package main

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/nkirsch/auto-builder/internal/notify"
	"github.com/nkirsch/auto-builder/internal/store"
)

type pushRecorder struct{ priorities []string }

func (r *pushRecorder) RoundTrip(req *http.Request) (*http.Response, error) {
	req.ParseForm()
	r.priorities = append(r.priorities, req.PostForm.Get("priority"))
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("{}")), Header: http.Header{}}, nil
}

func TestOutOfCredits(t *testing.T) {
	if !outOfCredits(`{"type":"result","is_error":true,"result":"Credit balance is too low"}`) {
		t.Error("missed Claude Code's credit error")
	}
	if !outOfCredits("Your credit balance is too low to access the Anthropic API.") {
		t.Error("missed the API's credit error")
	}
	if outOfCredits("runner: claude: exit status 1") {
		t.Error("flagged an unrelated failure")
	}
}

func TestAlertCreditsOncePerDay(t *testing.T) {
	rec := &pushRecorder{}
	orig := http.DefaultTransport
	http.DefaultTransport = rec
	defer func() { http.DefaultTransport = orig }()

	s := &server{storage: store.Dir(t.TempDir()), notify: notify.NewPushover("tok", "usr")}
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	s.alertCredits(ctx, now)
	s.alertCredits(ctx, now.Add(30*time.Minute))
	s.alertCredits(ctx, now.Add(25*time.Hour))
	if len(rec.priorities) != 2 || rec.priorities[0] != "1" {
		t.Errorf("sent %q, want two priority-1 alerts a day apart", rec.priorities)
	}
}
