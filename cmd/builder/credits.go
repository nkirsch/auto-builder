package main

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"time"
)

const (
	// creditsMarker holds when the last out-of-credits alert went out.
	creditsMarker = "alerts/credits-low"
	// creditsAlertEvery spaces the alerts: a failed run retries every
	// failBackoff, and each retry fails the same way until credit is added.
	creditsAlertEvery = 24 * time.Hour
	billingURL        = "https://console.anthropic.com/settings/billing"
)

// outOfCredits reports whether a run's output shows the Anthropic API
// refusing it for lack of credit.
func outOfCredits(output string) bool {
	return strings.Contains(strings.ToLower(output), "credit balance is too low")
}

// alertCredits pushes a high-priority alert, at most once per
// creditsAlertEvery; the marker lives in storage so restarts don't re-send.
func (s *server) alertCredits(ctx context.Context, now time.Time) {
	if rc, err := s.storage.Get(ctx, creditsMarker); err == nil {
		b, _ := io.ReadAll(rc)
		rc.Close()
		if last, err := time.Parse(time.RFC3339, strings.TrimSpace(string(b))); err == nil && now.Sub(last) < creditsAlertEvery {
			return
		}
	}
	if err := s.storage.Put(ctx, creditsMarker, "text/plain", strings.NewReader(now.UTC().Format(time.RFC3339))); err != nil {
		slog.Warn("credits marker", "err", err)
	}
	slog.Warn("out of Anthropic credits")
	s.notify.Alert(ctx, "Out of Anthropic credits",
		s.cfg.BaseURL+": the build agent can't run until credit is added.", billingURL)
}
