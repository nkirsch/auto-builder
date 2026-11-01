package board

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/nkirsch/auto-builder/internal/auth"
	"github.com/nkirsch/auto-builder/internal/db"
	"github.com/nkirsch/auto-builder/internal/pay"
)

type fakePay struct {
	got      pay.Checkout
	sessions map[string]pay.Session
}

func (f *fakePay) Name() string { return "Fake" }
func (f *fakePay) Checkout(_ context.Context, c pay.Checkout) (string, error) {
	f.got = c
	return "https://pay.example/c/1", nil
}
func (f *fakePay) Session(_ context.Context, id string) (pay.Session, error) {
	if s, ok := f.sessions[id]; ok {
		return s, nil
	}
	return pay.Session{}, errors.New("no such session")
}
func (f *fakePay) Webhook(*http.Request) (string, error) { return "", nil }

func creditReq(method, target, form string, m db.Member) *http.Request {
	r := httptest.NewRequest(method, target, strings.NewReader(form))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return r.WithContext(auth.WithMember(r.Context(), m))
}

func TestBuyCredit(t *testing.T) {
	ctx := context.Background()
	b, g := dbBoard(t)
	b.Slug = "www"
	ann := db.Member{Login: "ann", Role: "member", Email: "ann@example.com"}
	buy := func(amount string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		b.buyCredit(w, creditReq(http.MethodPost, "/credit", url.Values{"amount": {amount}}.Encode(), ann))
		return w
	}
	// no processor, then a processor but payments off: no checkout
	if w := buy("20"); !strings.Contains(w.Header().Get("Location"), "doesn") {
		t.Fatalf("without a processor: %s", w.Header().Get("Location"))
	}
	f := &fakePay{sessions: map[string]pay.Session{
		"cs_ok":    {ID: "cs_ok", Board: "www", Login: "ann", Cents: 2000, Paid: true},
		"cs_open":  {ID: "cs_open", Board: "www", Login: "ann", Cents: 2000},
		"cs_other": {ID: "cs_other", Board: "meet", Login: "ann", Cents: 2000, Paid: true},
	}}
	b.Pay = f
	if w := buy("20"); f.got.Cents != 0 || !strings.Contains(w.Header().Get("Location"), "doesn") {
		t.Fatal("checkout started with payments off")
	}
	b.Store.SetPaymentsOn(ctx, true)
	for _, bad := range []string{"", "4", "1001", "12.50", "abc"} {
		if buy(bad); f.got.Cents != 0 {
			t.Fatalf("amount %q started checkout", bad)
		}
	}
	w := buy("$20")
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "https://pay.example/c/1" {
		t.Fatalf("checkout redirect = %d %s", w.Code, w.Header().Get("Location"))
	}
	if f.got.Cents != 2000 || f.got.Board != "www" || f.got.Login != "ann" || f.got.Email != "ann@example.com" ||
		f.got.SuccessURL != "https://builder.example/credit/done?session={SESSION_ID}" {
		t.Errorf("checkout = %+v", f.got)
	}

	var paid []float64
	b.OnPaid = func(_ context.Context, _ string, usd float64) { paid = append(paid, usd) }
	done := func(id string) string {
		w := httptest.NewRecorder()
		b.creditDone(w, creditReq(http.MethodGet, "/credit/done?session="+id, "", ann))
		return w.Header().Get("Location")
	}
	if loc := done("cs_open"); !strings.Contains(loc, "hasn") {
		t.Errorf("unpaid: %s", loc)
	}
	if _, err := b.Credit(ctx, f.sessions["cs_other"]); err == nil {
		t.Error("credited another board's checkout")
	}
	if loc := done("cs_ok"); !strings.Contains(loc, "Thanks") {
		t.Errorf("paid: %s", loc)
	}
	done("cs_ok") // the return page reloaded, or the webhook arriving too
	if c, _ := b.Store.Credit30(ctx); c != 20 {
		t.Errorf("credit = %v, want 20 once", c)
	}
	if len(paid) != 1 || g.activity != 1 {
		t.Errorf("notified %v, gate activity %d; want once each", paid, g.activity)
	}
}

func TestCreditPage(t *testing.T) {
	ctx := context.Background()
	b, _ := dbBoard(t)
	if err := b.parse(); err != nil {
		t.Fatal(err)
	}
	ann := db.Member{Login: "ann", Role: "member"}
	get := func(m db.Member) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		b.credit(w, creditReq(http.MethodGet, "/credit", "", m))
		return w
	}
	if w := get(ann); w.Code != http.StatusNotFound {
		t.Fatalf("without a processor: %d", w.Code)
	}
	b.Pay = &fakePay{}
	b.Store.SetPaymentsOn(ctx, true)
	b.Store.RecordPayment(ctx, "cs_a", "ann", 10)
	b.Store.RecordPayment(ctx, "cs_b", "bo", 7)
	body := get(ann).Body.String()
	for _, want := range []string{"of <b>$37.00</b>", "Pay with Fake", "Your payments", "$10.00", `href="/credit"`} {
		if !strings.Contains(body, want) {
			t.Errorf("credit page missing %s", want)
		}
	}
	if strings.Contains(body, "$7.00") {
		t.Error("a member sees another's payment")
	}
	if body := get(db.Member{Login: "nk", Role: "admin"}).Body.String(); !strings.Contains(body, "$7.00") {
		t.Error("an admin doesn't see every payment")
	}
}

func TestSetPayments(t *testing.T) {
	ctx := context.Background()
	b, _ := dbBoard(t)
	admin := db.Member{Login: "nk", Role: "admin"}
	set := func(on string) {
		b.setPayments(httptest.NewRecorder(), creditReq(http.MethodPost, "/admin/payments", "on="+on, admin))
	}
	if set("true"); func() bool { on, _ := b.Store.PaymentsOn(ctx); return on }() {
		t.Fatal("turned on without a processor")
	}
	b.Pay = &fakePay{}
	set("true")
	if on, _ := b.Store.PaymentsOn(ctx); !on {
		t.Fatal("not turned on")
	}
	set("false")
	if on, _ := b.Store.PaymentsOn(ctx); on {
		t.Fatal("not turned off")
	}
}
