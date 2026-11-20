package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nkirsch/auto-builder/internal/billing"
	"github.com/nkirsch/auto-builder/internal/tenant"
)

// fakeStripe records calls and answers like Stripe's API.
type fakeStripe struct {
	mu    sync.Mutex
	calls []string
	lines []string
}

func (f *fakeStripe) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, r.URL.Path)
	switch r.URL.Path {
	case "/v1/customers":
		fmt.Fprint(w, `{"id": "cus_1"}`)
	case "/v1/invoices":
		fmt.Fprint(w, `{"id": "in_1"}`)
	case "/v1/invoiceitems":
		f.lines = append(f.lines, r.PostForm.Get("description")+"="+r.PostForm.Get("amount"))
		fmt.Fprint(w, `{"id": "ii"}`)
	default:
		fmt.Fprint(w, `{"id": "in_1", "hosted_invoice_url": "https://pay.example/in_1"}`)
	}
}

func TestBilling(t *testing.T) {
	ctx := context.Background()
	h := testHub(t)
	fs := &fakeStripe{}
	stripe := httptest.NewServer(fs)
	defer stripe.Close()
	now := time.Date(2026, 12, 4, 12, 0, 0, 0, billing.Pacific)
	nov := time.Date(2026, 11, 1, 0, 0, 0, 0, billing.Pacific)
	h.billing = &billingRT{label: "test", webhook: "whsec",
		stripe: &billing.Stripe{API: stripe.URL, Key: "sk", Client: stripe.Client()},
		cost: func(_ context.Context, m time.Time) (map[string]float64, error) {
			if m.Equal(nov) {
				return map[string]float64{"alpha": 0.40, "beta": 0.10, billing.RunnerValue: 3}, nil
			}
			return map[string]float64{}, nil
		}}
	srv := h.routes()

	// A creator's app bills to their personal account; a platform admin's own isn't billed.
	if err := h.reg.AddCreator(ctx, "ann@example.org", "nkirsch"); err != nil {
		t.Fatal(err)
	}
	if w := do(h, srv, "ann@example.org", "POST", "/new", url.Values{"slug": {"alpha"}, "name": {"Alpha"}}); w.Code != 303 {
		t.Fatalf("creator /new: %d %s", w.Code, w.Body)
	}
	if w := do(h, srv, "nkirsch", "POST", "/new", url.Values{"slug": {"beta"}, "repo": {"someone/beta"}, "existing": {"on"}}); w.Code != 303 {
		t.Fatalf("admin /new: %d %s", w.Code, w.Body)
	}
	alpha, _ := h.reg.Tenant(ctx, "alpha")
	beta, _ := h.reg.Tenant(ctx, "beta")
	acct, err := h.reg.BillingAccount(ctx, alpha.BillingAccount)
	if err != nil || acct.Kind != "personal" || acct.Identity != "ann@example.org" || acct.Email != "ann@example.org" || beta.BillingAccount != 0 {
		t.Fatalf("alpha account %+v %v; beta %d", acct, err, beta.BillingAccount)
	}

	// Runs in November: alpha 100s and $1.00 of AI, beta 200s and $0.50.
	for slug, r := range map[string]struct {
		secs int
		cost float64
	}{"alpha": {100, 1}, "beta": {200, 0.5}} {
		st, _ := h.reg.Store(ctx, slug)
		id, err := st.CreateRun(ctx, "manual", "nkirsch")
		if err != nil {
			t.Fatal(err)
		}
		at := nov.AddDate(0, 0, 10)
		if _, err := st.Pool.Exec(ctx, `UPDATE runs SET status = 'done', cost_usd = $4, created_at = $2, started_at = $2, finished_at = $3
			WHERE id = $1`, id, at, at.Add(time.Duration(r.secs)*time.Second), r.cost); err != nil {
			t.Fatal(err)
		}
	}

	// Platform admins only; others don't even learn it exists.
	if w := do(h, srv, "ann@example.org", "GET", "/admin/billing", nil); w.Code != 403 {
		t.Fatalf("creator /admin/billing: %d", w.Code)
	}
	if w := do(h, srv, "nkirsch", "POST", "/admin/billing/terms", url.Values{"markup": {"10"}, "fee": {"2"}}); w.Code != 303 {
		t.Fatalf("terms: %d", w.Code)
	}
	if err := h.computeBilling(ctx, now); err != nil {
		t.Fatal(err)
	}
	stmts, err := h.reg.Statements(ctx, nov)
	// December's draft is alpha's monthly fee alone.
	if err != nil || len(stmts) != 3 || stmts[0].Status != "draft" || stmts[0].TotalCents != 200 {
		t.Fatalf("statements %+v %v", stmts, err)
	}
	a, b := stmts[1], stmts[2]
	// alpha: AI $1.00, runner $1.00 (a third of $3), cloud $0.40; +10% and a $2 fee.
	if a.Tenant != "alpha" || a.Status != "final" || a.AICents != 110 || a.RunnerCents != 110 || a.CloudCents != 44 ||
		a.FeeCents != 200 || a.TotalCents != 464 || a.RunSeconds != 100 {
		t.Fatalf("alpha %+v", a)
	}
	// beta isn't billed: raw cost, no fee.
	if b.Tenant != "beta" || b.Account != 0 || b.AICents != 50 || b.RunnerCents != 200 || b.CloudCents != 10 || b.FeeCents != 0 {
		t.Fatalf("beta %+v", b)
	}
	// Final statements don't change on recompute.
	h.reg.SetBillingTerms(ctx, 50, 0)
	h.computeBilling(ctx, now)
	if s, _ := h.reg.Statements(ctx, nov); s[1].TotalCents != 464 {
		t.Fatalf("final recomputed: %+v", s[1])
	}

	w := do(h, srv, "nkirsch", "GET", "/admin/billing", nil)
	for _, want := range []string{"ann@example.org", "$4.64", "November 2026", "not billed"} {
		if !strings.Contains(w.Body.String(), want) {
			t.Fatalf("/admin/billing lacks %q: %.400s", want, w.Body)
		}
	}

	// Send puts alpha's lines on one invoice and marks it invoiced.
	w = do(h, srv, "nkirsch", "POST", fmt.Sprintf("/admin/billing/accounts/%d/send", acct.ID), url.Values{})
	if loc := w.Header().Get("Location"); w.Code != 303 || !strings.Contains(loc, "in_1") {
		t.Fatalf("send: %d %s", w.Code, loc)
	}
	if got := strings.Join(fs.lines, "; "); got != "alpha · Nov 2026 · AI agent=110; alpha · Nov 2026 · agent runner=110; alpha · Nov 2026 · cloud hosting=44; alpha · Nov 2026 · monthly fee=200" {
		t.Fatalf("lines %s", got)
	}
	if s, _ := h.reg.Statements(ctx, nov); s[1].Status != "invoiced" || s[1].StripeInvoice != "in_1" || s[0].Status != "draft" {
		t.Fatalf("after send %+v", s)
	}
	if a, _ := h.reg.BillingAccount(ctx, acct.ID); a.StripeCustomer != "cus_1" {
		t.Fatalf("customer %q", a.StripeCustomer)
	}
	// Nothing left: a second Send refuses.
	n := len(fs.calls)
	do(h, srv, "nkirsch", "POST", fmt.Sprintf("/admin/billing/accounts/%d/send", acct.ID), url.Values{})
	if len(fs.calls) != n {
		t.Fatalf("second send called Stripe: %v", fs.calls[n:])
	}

	// Stripe's webhook marks it paid; a bad signature is refused.
	body := `{"type": "invoice.paid", "data": {"object": {"id": "in_1", "object": "invoice"}}}`
	ts := fmt.Sprint(time.Now().Unix())
	mac := hmac.New(sha256.New, []byte("whsec"))
	mac.Write([]byte(ts + "." + body))
	for sig, code := range map[string]int{"t=" + ts + ",v1=00": 400, "t=" + ts + ",v1=" + hex.EncodeToString(mac.Sum(nil)): 204} {
		r := httptest.NewRequest("POST", "/api/stripe/webhook", strings.NewReader(body))
		r.Header.Set("Stripe-Signature", sig)
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, r)
		if rec.Code != code {
			t.Fatalf("webhook %s: %d %s", sig, rec.Code, rec.Body)
		}
	}
	if s, _ := h.reg.Statements(ctx, nov); s[1].Status != "paid" {
		t.Fatalf("after webhook %+v", s[1])
	}

	// Managed accounts take any address and can be assigned an existing app.
	if w := do(h, srv, "nkirsch", "POST", "/admin/billing/accounts", url.Values{"kind": {"managed"}, "name": {"Kirsch LLC"},
		"email": {"Pay@Example.org"}, "app": {"beta"}}); w.Code != 303 {
		t.Fatalf("add managed: %d", w.Code)
	}
	beta, _ = h.reg.Tenant(ctx, "beta")
	if m, err := h.reg.BillingAccount(ctx, beta.BillingAccount); err != nil || m.Kind != "managed" || m.Email != "pay@example.org" ||
		len(m.Apps) != 1 || m.Apps[0] != "beta" {
		t.Fatalf("managed %+v %v", m, err)
	}
	if _, err := h.reg.CreateAccount(ctx, tenant.Account{Kind: "managed"}); err == nil {
		t.Fatal("blank managed account created")
	}
}

func TestBillingOff(t *testing.T) {
	h := testHub(t)
	srv := h.routes()
	if w := do(h, srv, "nkirsch", "GET", "/admin/billing", nil); w.Code != 404 {
		t.Fatalf("/admin/billing without BILLING: %d", w.Code)
	}
	if w := do(h, srv, "nkirsch", "GET", "/admin", nil); strings.Contains(w.Body.String(), "/admin/billing") {
		t.Fatal("/admin links billing without BILLING")
	}
}
