// Package notify sends the admin push messages through signal.nick.org when
// SIGNAL_URL is set; in-app notifications live in the notifications table.
package notify

import (
	"context"
	"log/slog"
	"strings"
	"time"

	signal "github.com/nkirsch/signal-go"
)

// Notifier sends to the admin through signal. Without a signal URL it only logs.
type Notifier struct {
	signal *signal.Client
}

// New sends through signal at signalURL (bearer SIGNAL_TOKEN, else the
// Cloud Run identity), or only logs when signalURL is empty.
func New(signalURL string) *Notifier {
	p := &Notifier{}
	if signalURL != "" {
		p.signal = signal.NewFromEnv()
		p.signal.URL = strings.TrimRight(signalURL, "/")
	}
	return p
}

// Send posts a message; failures are logged, never returned, so callers aren't held up.
func (p *Notifier) Send(ctx context.Context, title, message, link string) {
	p.send(ctx, title, message, link, 0)
}

// Alert is Send at high priority, for something that needs action.
func (p *Notifier) Alert(ctx context.Context, title, message, link string) {
	p.send(ctx, title, message, link, 1)
}

func (p *Notifier) send(ctx context.Context, title, message, link string, priority int) {
	if p != nil && p.signal != nil {
		event := "board.notify"
		if priority > 0 {
			event = "board.alert"
		}
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		p.signal.Send(ctx, signal.Event{Event: event, Title: title, Message: message, URL: link, App: "deployments", Priority: signal.P(priority)})
		return
	}
	slog.Info("notify", "title", title, "message", message, "url", link)
}
