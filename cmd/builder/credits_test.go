package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/nkirsch/auto-builder/internal/notify"
	"github.com/nkirsch/auto-builder/internal/store"
)

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
	var priorities []float64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var ev struct {
			Priority float64 `json:"priority"`
		}
		json.NewDecoder(r.Body).Decode(&ev)
		priorities = append(priorities, ev.Priority)
		w.Write([]byte(`{"status":"delivered","delivered":true}`))
	}))
	defer srv.Close()
	t.Setenv("SIGNAL_TOKEN", "tok")

	s := &server{storage: store.Dir(t.TempDir()), notify: notify.New(srv.URL)}
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	s.alertCredits(ctx, now)
	s.alertCredits(ctx, now.Add(30*time.Minute))
	s.alertCredits(ctx, now.Add(25*time.Hour))
	if len(priorities) != 2 || priorities[0] != 1 {
		t.Errorf("sent %v, want two priority-1 alerts a day apart", priorities)
	}
}
