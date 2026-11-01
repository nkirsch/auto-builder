package board

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/nkirsch/auto-builder/internal/auth"
	"github.com/nkirsch/auto-builder/internal/db"
	"github.com/nkirsch/auto-builder/internal/pay"
)

// Credit purchases are whole dollars in this range.
const (
	minCredit = 5
	maxCredit = 1000
)

// payments reports whether members can buy credit here: the hub has a
// processor and the board's admins turned payments on.
func (b *Board) payments(ctx context.Context) bool {
	if b.Pay == nil {
		return false
	}
	on, err := b.Store.PaymentsOn(ctx)
	if err != nil {
		slog.Warn("payments setting", "err", err)
	}
	return on
}

func (b *Board) credit(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if !b.payments(ctx) {
		http.NotFound(w, r)
		return
	}
	costs, err := b.Store.RunCosts(ctx)
	if err != nil {
		b.fail(w, err)
		return
	}
	limit, err := b.Store.SpendLimit(ctx)
	if err != nil {
		b.fail(w, err)
		return
	}
	credit, err := b.Store.Credit30(ctx)
	if err != nil {
		b.fail(w, err)
		return
	}
	paid, err := b.Store.Payments(ctx, 50)
	if err != nil {
		b.fail(w, err)
		return
	}
	me := auth.Member(ctx)
	if !me.IsAdmin() { // members see their own payments only
		paid = slices.DeleteFunc(paid, func(p db.Payment) bool { return !strings.EqualFold(p.Login, me.Login) })
	}
	b.render(w, r, "credit", "Credit", map[string]any{"Spent": costs.Last30Days, "Limit": limit, "Credit": credit,
		"Total": limit + credit, "Payments": paid, "Processor": b.Pay.Name(), "Min": minCredit, "Max": maxCredit})
}

func (b *Board) buyCredit(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if !b.payments(ctx) {
		redirect(w, r, "/", "This board doesn't take payments")
		return
	}
	v, err := strconv.ParseFloat(strings.TrimPrefix(strings.TrimSpace(r.FormValue("amount")), "$"), 64)
	if err != nil || v != math.Trunc(v) || v < minCredit || v > maxCredit {
		redirect(w, r, "/credit", fmt.Sprintf("Enter whole dollars, $%d to $%d", minCredit, maxCredit))
		return
	}
	me := auth.Member(ctx)
	name := b.Name
	if name == "" {
		name = b.Repo
	}
	base := strings.TrimSuffix(b.BaseURL, "/")
	u, err := b.Pay.Checkout(ctx, pay.Checkout{Board: b.Slug, Login: me.Login, Email: me.Email,
		Product: "Build credit: " + name, Cents: int64(v) * 100,
		SuccessURL: base + "/credit/done?session={SESSION_ID}", CancelURL: base + "/credit"})
	if err != nil {
		slog.Error("checkout", "err", err)
		redirect(w, r, "/credit", "Couldn't start checkout: "+err.Error())
		return
	}
	http.Redirect(w, r, u, http.StatusSeeOther)
}

// creditDone is where the processor returns a member after checkout.
func (b *Board) creditDone(w http.ResponseWriter, r *http.Request) {
	if b.Pay == nil {
		http.NotFound(w, r)
		return
	}
	s, err := b.Pay.Session(r.Context(), r.URL.Query().Get("session"))
	if err != nil {
		slog.Warn("checkout session", "err", err)
		redirect(w, r, "/credit", "Couldn't confirm the payment yet; it shows here once "+b.Pay.Name()+" reports it")
		return
	}
	if _, err := b.Credit(r.Context(), s); errors.Is(err, ErrNotPaid) {
		redirect(w, r, "/credit", "The payment hasn't gone through yet; it shows here once it does")
		return
	} else if err != nil {
		b.fail(w, err)
		return
	}
	redirect(w, r, "/credit", fmt.Sprintf("Thanks: $%.2f added to the spend limit for 30 days", float64(s.Cents)/100))
}

// ErrNotPaid is a checkout that hasn't been paid (yet).
var ErrNotPaid = errors.New("checkout not paid")

// Credit records a paid checkout for this board once, from the return page
// or the processor's webhook; it reports whether this call recorded it.
func (b *Board) Credit(ctx context.Context, s pay.Session) (bool, error) {
	if !s.Paid || s.Cents <= 0 {
		return false, ErrNotPaid
	}
	if s.Board != b.Slug {
		return false, fmt.Errorf("checkout %s is for board %q, not %q", s.ID, s.Board, b.Slug)
	}
	usd := float64(s.Cents) / 100
	added, err := b.Store.RecordPayment(ctx, s.ID, s.Login, usd)
	if err != nil || !added {
		return false, err
	}
	slog.Info("payment", "board", b.Slug, "login", s.Login, "usd", usd, "checkout", s.ID)
	if b.OnPaid != nil {
		b.OnPaid(ctx, s.Login, usd)
	}
	b.Gate.Activity() // more credit may release held work
	return true, nil
}

func (b *Board) setPayments(w http.ResponseWriter, r *http.Request) {
	if b.Pay == nil {
		redirect(w, r, "/admin", "Payments need a processor on the hub first")
		return
	}
	on := r.FormValue("on") == "true"
	if err := b.Store.SetPaymentsOn(r.Context(), on); err != nil {
		b.fail(w, err)
		return
	}
	msg := "Members can no longer buy credit"
	if on {
		msg = "Members can buy credit on the Credit page"
	}
	redirect(w, r, "/admin", msg)
}
