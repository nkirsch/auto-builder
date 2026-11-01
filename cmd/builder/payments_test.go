package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/nkirsch/auto-builder/internal/board"
	"github.com/nkirsch/auto-builder/internal/db"
	"github.com/nkirsch/auto-builder/internal/pay"
)

type stubPay struct {
	id  string
	err error
}

func (s stubPay) Name() string                                           { return "Stub" }
func (s stubPay) Checkout(context.Context, pay.Checkout) (string, error) { return "", nil }
func (s stubPay) Session(_ context.Context, id string) (pay.Session, error) {
	return pay.Session{ID: id, Board: "gone", Paid: true, Cents: 100}, nil
}
func (s stubPay) Webhook(*http.Request) (string, error) { return s.id, s.err }

func TestPayWebhook(t *testing.T) {
	var asked string
	boardFor := func(_ context.Context, slug string) (*board.Board, error) {
		asked = slug
		return nil, db.ErrNotFound
	}
	for name, tc := range map[string]struct {
		p    stubPay
		want int
	}{
		"bad signature": {stubPay{err: errors.New("bad signature")}, http.StatusBadRequest},
		"ignored event": {stubPay{}, http.StatusNoContent},
		"deleted board": {stubPay{id: "cs_1"}, http.StatusNoContent},
	} {
		asked = ""
		w := httptest.NewRecorder()
		payWebhook(tc.p, boardFor)(w, httptest.NewRequest(http.MethodPost, "/api/pay/webhook", nil))
		if w.Code != tc.want {
			t.Errorf("%s: status %d, want %d", name, w.Code, tc.want)
		}
		if (tc.p.id != "") != (asked == "gone") {
			t.Errorf("%s: looked up board %q", name, asked)
		}
	}
}
