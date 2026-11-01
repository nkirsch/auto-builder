// Package pay takes payments for agent credit through a hosted checkout. A
// hub picks the processor by its secrets; Stripe is the one so far.
package pay

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Checkout is one purchase of credit for a board.
type Checkout struct {
	Board      string // tenant slug, "" on a single board
	Login      string // the member paying
	Email      string // prefills the processor's form; may be empty
	Product    string // line item shown on the processor's page
	Cents      int64
	SuccessURL string // may hold {SESSION_ID}, replaced by the processor's session id
	CancelURL  string
}

// Session is a checkout as the processor reports it.
type Session struct {
	ID    string
	Board string
	Login string
	Cents int64
	Paid  bool
}

// Processor creates hosted checkouts and reports on them.
type Processor interface {
	Name() string
	// Checkout returns the processor's page to send the member to.
	Checkout(ctx context.Context, c Checkout) (string, error)
	// Session looks a checkout up with the processor, the source of truth.
	Session(ctx context.Context, id string) (Session, error)
	// Webhook verifies a webhook request and returns the checkout it
	// completes, or "" for events that don't complete one.
	Webhook(r *http.Request) (string, error)
}

// FromEnv is the configured processor, or nil when payments are off.
func FromEnv(stripeKey, stripeWebhookSecret string) Processor {
	if stripeKey == "" {
		return nil
	}
	return &Stripe{Key: stripeKey, WebhookSecret: stripeWebhookSecret}
}

// Stripe uses Stripe Checkout in payment mode, in US dollars.
type Stripe struct {
	Key           string
	WebhookSecret string // "" leaves webhooks off; the return page still records payments
	API           string // tests: instead of https://api.stripe.com
	Client        *http.Client
	Now           func() time.Time // tests
}

func (s *Stripe) Name() string { return "Stripe" }

func (s *Stripe) Checkout(ctx context.Context, c Checkout) (string, error) {
	if c.Cents <= 0 {
		return "", errors.New("amount must be positive")
	}
	f := url.Values{
		"mode":                                   {"payment"},
		"success_url":                            {strings.ReplaceAll(c.SuccessURL, "{SESSION_ID}", "{CHECKOUT_SESSION_ID}")},
		"cancel_url":                             {c.CancelURL},
		"client_reference_id":                    {c.Login},
		"metadata[board]":                        {c.Board},
		"metadata[login]":                        {c.Login},
		"line_items[0][quantity]":                {"1"},
		"line_items[0][price_data][currency]":    {"usd"},
		"line_items[0][price_data][unit_amount]": {strconv.FormatInt(c.Cents, 10)},
		"line_items[0][price_data][product_data][name]": {c.Product},
	}
	if c.Email != "" {
		f.Set("customer_email", c.Email)
	}
	var out struct {
		URL string `json:"url"`
	}
	if err := s.call(ctx, http.MethodPost, "/v1/checkout/sessions", f, &out); err != nil {
		return "", err
	}
	if out.URL == "" {
		return "", errors.New("stripe: checkout has no url")
	}
	return out.URL, nil
}

func (s *Stripe) Session(ctx context.Context, id string) (Session, error) {
	if !strings.HasPrefix(id, "cs_") || strings.ContainsAny(id, "/?#") {
		return Session{}, errors.New("not a checkout session id")
	}
	var out struct {
		ID            string            `json:"id"`
		PaymentStatus string            `json:"payment_status"`
		AmountTotal   int64             `json:"amount_total"`
		Currency      string            `json:"currency"`
		Metadata      map[string]string `json:"metadata"`
	}
	if err := s.call(ctx, http.MethodGet, "/v1/checkout/sessions/"+id, nil, &out); err != nil {
		return Session{}, err
	}
	return Session{ID: out.ID, Board: out.Metadata["board"], Login: out.Metadata["login"], Cents: out.AmountTotal,
		Paid: out.PaymentStatus == "paid" && out.Currency == "usd"}, nil
}

// signatureTolerance is how old a webhook's signed timestamp may be.
const signatureTolerance = 5 * time.Minute

func (s *Stripe) Webhook(r *http.Request) (string, error) {
	if s.WebhookSecret == "" {
		return "", errors.New("stripe: no webhook secret")
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if err := s.verify(r.Header.Get("Stripe-Signature"), body); err != nil {
		return "", err
	}
	var ev struct {
		Type string `json:"type"`
		Data struct {
			Object struct {
				ID string `json:"id"`
			} `json:"object"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &ev); err != nil {
		return "", fmt.Errorf("stripe: event: %w", err)
	}
	switch ev.Type {
	case "checkout.session.completed", "checkout.session.async_payment_succeeded":
		return ev.Data.Object.ID, nil
	}
	return "", nil
}

// verify checks Stripe's "t=<unix>,v1=<hex hmac>" header against the body.
func (s *Stripe) verify(header string, body []byte) error {
	var ts string
	var sigs []string
	for _, part := range strings.Split(header, ",") {
		k, v, _ := strings.Cut(strings.TrimSpace(part), "=")
		switch k {
		case "t":
			ts = v
		case "v1":
			sigs = append(sigs, v)
		}
	}
	t, err := strconv.ParseInt(ts, 10, 64)
	if err != nil || len(sigs) == 0 {
		return errors.New("stripe: malformed signature")
	}
	now := time.Now
	if s.Now != nil {
		now = s.Now
	}
	if d := now().Sub(time.Unix(t, 0)); d > signatureTolerance || d < -signatureTolerance {
		return errors.New("stripe: signature too old")
	}
	mac := hmac.New(sha256.New, []byte(s.WebhookSecret))
	mac.Write([]byte(ts + "."))
	mac.Write(body)
	want := mac.Sum(nil)
	for _, sig := range sigs {
		if got, err := hex.DecodeString(sig); err == nil && hmac.Equal(got, want) {
			return nil
		}
	}
	return errors.New("stripe: bad signature")
}

func (s *Stripe) call(ctx context.Context, method, path string, form url.Values, out any) error {
	api := s.API
	if api == "" {
		api = "https://api.stripe.com"
	}
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, api+path, body)
	if err != nil {
		return err
	}
	req.SetBasicAuth(s.Key, "")
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	client := s.Client
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		json.Unmarshal(b, &e)
		return fmt.Errorf("stripe: %s: %s", resp.Status, e.Error.Message)
	}
	return json.Unmarshal(b, out)
}

// Sign makes a Stripe-Signature header for body, for tests and local webhooks.
func Sign(secret string, t time.Time, body []byte) string {
	ts := strconv.FormatInt(t.Unix(), 10)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts + "."))
	mac.Write(body)
	return "t=" + ts + ",v1=" + hex.EncodeToString(mac.Sum(nil))
}
