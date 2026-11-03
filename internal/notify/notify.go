// Package notify sends the admin push messages, through signal.nick.org when
// SIGNAL_URL is set (Pushover directly otherwise); in-app notifications live
// in the notifications table.
package notify

import (
	"context"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	signal "github.com/nkirsch/signal-go"
)

// Pushover sends to one user, through signal when configured. With neither
// signal nor Pushover credentials it only logs.
type Pushover struct {
	Token, User string
	client      *http.Client
	signal      *signal.Client
}

// NewPushover posts to Pushover directly.
func NewPushover(token, user string) *Pushover {
	return &Pushover{Token: token, User: user, client: &http.Client{Timeout: 10 * time.Second}}
}

// New sends through signal at signalURL (bearer SIGNAL_TOKEN, else the
// Cloud Run identity) when signalURL is set, else Pushover directly.
func New(signalURL, token, user string) *Pushover {
	p := NewPushover(token, user)
	if signalURL != "" {
		p.signal = signal.NewFromEnv()
		p.signal.URL = strings.TrimRight(signalURL, "/")
	}
	return p
}

// Send posts a message; failures are logged, never returned, so callers aren't held up.
func (p *Pushover) Send(ctx context.Context, title, message, link string) {
	p.send(ctx, title, message, link, 0)
}

// Alert is Send at high priority, for something that needs action.
func (p *Pushover) Alert(ctx context.Context, title, message, link string) {
	p.send(ctx, title, message, link, 1)
}

func (p *Pushover) send(ctx context.Context, title, message, link string, priority int) {
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
	if p == nil || p.Token == "" || p.User == "" {
		slog.Info("notify", "title", title, "message", message, "url", link)
		return
	}
	form := url.Values{"token": {p.Token}, "user": {p.User}, "title": {title}, "message": {message}}
	if link != "" {
		form.Set("url", link)
	}
	if priority != 0 {
		form.Set("priority", strconv.Itoa(priority))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.pushover.net/1/messages.json",
		strings.NewReader(form.Encode()))
	if err != nil {
		slog.Warn("pushover", "err", err)
		return
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := p.client.Do(req)
	if err != nil {
		slog.Warn("pushover", "err", err)
		return
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		slog.Warn("pushover", "status", resp.Status)
	}
}
