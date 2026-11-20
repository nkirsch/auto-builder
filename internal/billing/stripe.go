package billing

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

// Stripe is the few calls invoicing needs, over Stripe's form-encoded REST API.
type Stripe struct {
	API    string // "https://api.stripe.com", overridable in tests
	Key    string // secret key
	Client *http.Client
}

// call POSTs form to path with an idempotency key, decoding the object into out.
func (s *Stripe) call(ctx context.Context, path, idem string, form url.Values, out any) error {
	api := cmpOr(s.API, "https://api.stripe.com")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, api+path, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.SetBasicAuth(s.Key, "")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if idem != "" {
		req.Header.Set("Idempotency-Key", idem)
	}
	c := s.Client
	if c == nil {
		c = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	msg, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("stripe %s: %s: %w", path, resp.Status, err)
	}
	if resp.StatusCode/100 != 2 {
		var e struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(msg, &e)
		return fmt.Errorf("stripe %s: %s: %s", path, resp.Status, e.Error.Message)
	}
	if out != nil {
		return json.Unmarshal(msg, out)
	}
	return nil
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// Customer makes a customer for a billing account; idem keeps a retry from making two.
func (s *Stripe) Customer(ctx context.Context, idem, name, email string) (string, error) {
	var c struct{ ID string }
	err := s.call(ctx, "/v1/customers", idem, url.Values{"name": {name}, "email": {email}}, &c)
	return c.ID, err
}

// Line is one invoice item.
type Line struct {
	Description string
	Cents       int64
}

// Invoice is a sent invoice.
type Invoice struct {
	ID  string `json:"id"`
	URL string `json:"hosted_invoice_url"`
}

// Draft makes an empty draft invoice to be emailed, due in days.
func (s *Stripe) Draft(ctx context.Context, idem, customer, description string, days int) (string, error) {
	var inv Invoice
	err := s.call(ctx, "/v1/invoices", idem, url.Values{
		"customer":                       {customer},
		"collection_method":              {"send_invoice"},
		"days_until_due":                 {strconv.Itoa(days)},
		"auto_advance":                   {"false"},
		"pending_invoice_items_behavior": {"exclude"},
		"description":                    {description},
		"currency":                       {"usd"},
	}, &inv)
	return inv.ID, err
}

// AddLine adds an item to a draft invoice; idem makes a retry add it once.
func (s *Stripe) AddLine(ctx context.Context, idem, customer, invoice string, l Line) error {
	return s.call(ctx, "/v1/invoiceitems", idem, url.Values{
		"customer":    {customer},
		"invoice":     {invoice},
		"amount":      {strconv.FormatInt(l.Cents, 10)},
		"currency":    {"usd"},
		"description": {l.Description},
	}, nil)
}

// Send finalizes a draft invoice and emails it.
func (s *Stripe) Send(ctx context.Context, invoice string) (Invoice, error) {
	var inv Invoice
	if err := s.call(ctx, "/v1/invoices/"+invoice+"/finalize", "finalize-"+invoice, url.Values{}, &inv); err != nil {
		return inv, err
	}
	err := s.call(ctx, "/v1/invoices/"+invoice+"/send", "send-"+invoice, url.Values{}, &inv)
	return inv, err
}

// webhookTolerance bounds a webhook's age, against replays.
const webhookTolerance = 5 * time.Minute

// Event is a verified webhook event about an invoice.
type Event struct {
	Type    string
	Invoice string
}

// ParseWebhook verifies a webhook body against its Stripe-Signature header
// (t=<unix>,v1=<hex HMAC-SHA256 of "t.body">) and returns its invoice event.
func ParseWebhook(body []byte, header, secret string, now time.Time) (Event, error) {
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
		return Event{}, errors.New("malformed signature")
	}
	if d := now.Sub(time.Unix(t, 0)); d > webhookTolerance || d < -webhookTolerance {
		return Event{}, errors.New("signature too old")
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts + "."))
	mac.Write(body)
	want := mac.Sum(nil)
	ok := false
	for _, s := range sigs {
		if got, err := hex.DecodeString(s); err == nil && hmac.Equal(got, want) {
			ok = true
		}
	}
	if !ok {
		return Event{}, errors.New("bad signature")
	}
	var e struct {
		Type string `json:"type"`
		Data struct {
			Object struct {
				ID     string `json:"id"`
				Object string `json:"object"`
			} `json:"object"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &e); err != nil {
		return Event{}, err
	}
	ev := Event{Type: e.Type}
	if e.Data.Object.Object == "invoice" {
		ev.Invoice = e.Data.Object.ID
	}
	return ev, nil
}

// InvoiceStatus is the statement status an invoice event moves to, or "".
func InvoiceStatus(eventType string) string {
	switch eventType {
	case "invoice.paid":
		return "paid"
	case "invoice.voided":
		return "void"
	case "invoice.marked_uncollectible":
		return "uncollectible"
	}
	return ""
}
