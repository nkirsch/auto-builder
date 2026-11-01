package pay

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestStripeCheckoutAndSession(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if key, _, _ := r.BasicAuth(); key != "sk_test" {
			http.Error(w, `{"error":{"message":"bad key"}}`, http.StatusUnauthorized)
			return
		}
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/checkout/sessions":
			r.ParseForm()
			for k, want := range map[string]string{
				"mode": "payment", "metadata[board]": "www", "metadata[login]": "ann",
				"line_items[0][price_data][unit_amount]": "2500", "line_items[0][price_data][currency]": "usd",
				"success_url": "https://b.example/www/pay/done?session={CHECKOUT_SESSION_ID}",
			} {
				if got := r.PostForm.Get(k); got != want {
					t.Errorf("%s = %q, want %q", k, got, want)
				}
			}
			json.NewEncoder(w).Encode(map[string]string{"id": "cs_1", "url": "https://checkout.stripe.com/c/cs_1"})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/checkout/sessions/cs_1":
			json.NewEncoder(w).Encode(map[string]any{"id": "cs_1", "payment_status": "paid", "amount_total": 2500,
				"currency": "usd", "metadata": map[string]string{"board": "www", "login": "ann"}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	s := &Stripe{Key: "sk_test", API: srv.URL}
	ctx := context.Background()
	u, err := s.Checkout(ctx, Checkout{Board: "www", Login: "ann", Product: "credit", Cents: 2500,
		SuccessURL: "https://b.example/www/pay/done?session={SESSION_ID}", CancelURL: "https://b.example/www/credit"})
	if err != nil || u != "https://checkout.stripe.com/c/cs_1" {
		t.Fatalf("checkout = %q, %v", u, err)
	}
	got, err := s.Session(ctx, "cs_1")
	if err != nil {
		t.Fatal(err)
	}
	if want := (Session{ID: "cs_1", Board: "www", Login: "ann", Cents: 2500, Paid: true}); got != want {
		t.Errorf("session = %+v, want %+v", got, want)
	}
	if _, err := s.Session(ctx, "../v1/charges"); err == nil {
		t.Error("session accepted a path")
	}
	if _, err := (&Stripe{Key: "nope", API: srv.URL}).Session(ctx, "cs_1"); err == nil || !strings.Contains(err.Error(), "bad key") {
		t.Errorf("bad key err = %v", err)
	}
}

func TestStripeWebhook(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	s := &Stripe{WebhookSecret: "whsec", Now: func() time.Time { return now }}
	body := []byte(`{"type":"checkout.session.completed","data":{"object":{"id":"cs_9"}}}`)
	req := func(sig string, body []byte) *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/api/pay/webhook", strings.NewReader(string(body)))
		r.Header.Set("Stripe-Signature", sig)
		return r
	}
	if id, err := s.Webhook(req(Sign("whsec", now, body), body)); err != nil || id != "cs_9" {
		t.Fatalf("webhook = %q, %v", id, err)
	}
	for name, r := range map[string]*http.Request{
		"wrong secret": req(Sign("other", now, body), body),
		"stale":        req(Sign("whsec", now.Add(-10*time.Minute), body), body),
		"tampered":     req(Sign("whsec", now, body), []byte(strings.Replace(string(body), "cs_9", "cs_8", 1))),
		"no header":    req("", body),
	} {
		if _, err := s.Webhook(r); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	other := []byte(`{"type":"charge.refunded","data":{"object":{"id":"ch_1"}}}`)
	if id, err := s.Webhook(req(Sign("whsec", now, other), other)); err != nil || id != "" {
		t.Errorf("other event = %q, %v", id, err)
	}
}

func TestFromEnv(t *testing.T) {
	if FromEnv("", "x") != nil {
		t.Error("no key should mean payments off")
	}
	if p := FromEnv("sk", ""); p == nil || p.Name() != "Stripe" {
		t.Errorf("FromEnv = %v", p)
	}
}
