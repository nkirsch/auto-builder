package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/nkirsch/auto-builder/internal/board"
	"github.com/nkirsch/auto-builder/internal/db"
	"github.com/nkirsch/auto-builder/internal/notify"
	"github.com/nkirsch/auto-builder/internal/pay"
)

// payWebhook records checkouts the processor reports complete, on the board
// named in the checkout (boardFor); unknown boards are logged and acknowledged.
func payWebhook(p pay.Processor, boardFor func(ctx context.Context, slug string) (*board.Board, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := p.Webhook(r)
		if err != nil {
			slog.Warn("payment webhook", "err", err)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if id == "" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		s, err := p.Session(r.Context(), id)
		if err != nil {
			slog.Error("payment webhook: session", "checkout", id, "err", err)
			http.Error(w, "lookup failed", http.StatusBadGateway) // the processor retries
			return
		}
		b, err := boardFor(r.Context(), s.Board)
		if errors.Is(err, db.ErrNotFound) {
			slog.Error("payment for an unknown board", "board", s.Board, "checkout", id, "login", s.Login, "cents", s.Cents)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if err == nil {
			_, err = b.Credit(r.Context(), s)
		}
		if err != nil && !errors.Is(err, board.ErrNotPaid) {
			slog.Error("payment webhook: record", "board", s.Board, "checkout", id, "err", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// notifyPaid Pushovers each payment to the hub's operator.
func notifyPaid(p *notify.Pushover, where string) func(ctx context.Context, login string, usd float64) {
	return func(ctx context.Context, login string, usd float64) {
		p.Send(ctx, "Payment received", fmt.Sprintf("%s paid $%.2f for credit on %s", login, usd, where), "")
	}
}
