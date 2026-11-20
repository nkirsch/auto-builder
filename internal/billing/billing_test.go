package billing

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestMonth(t *testing.T) {
	// 07:30 UTC on Dec 1 is still Nov 30 in Pacific.
	got := Month(time.Date(2026, 12, 1, 7, 30, 0, 0, time.UTC))
	if got.Format("2006-01-02") != "2026-11-01" || got.Location() != Pacific {
		t.Fatalf("Month = %v", got)
	}
}

func TestPrice(t *testing.T) {
	p := Price(Costs{AI: 1.234, Runner: 0.1, Cloud: -0.5}, Terms{MarkupPct: 10, BaseFee: 2})
	if p != (Priced{AI: 136, Runner: 11, Cloud: 0, Fee: 200, Total: 347}) {
		t.Fatalf("Price = %+v", p)
	}
	if p := Price(Costs{AI: 0.004}, Terms{}); p.Total != 0 {
		t.Fatalf("sub-cent = %+v", p)
	}
}

func TestSplitRunner(t *testing.T) {
	s := SplitRunner(3, map[string]int64{"a": 100, "b": 200, "c": 0})
	if s["a"] != 1 || s["b"] != 2 || s["c"] != 0 {
		t.Fatalf("split = %v", s)
	}
	if s := SplitRunner(3, map[string]int64{"a": 0}); len(s) != 0 {
		t.Fatalf("no runs = %v", s)
	}
}

func TestExportCost(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/bigquery/v2/projects/billing-proj/queries" {
			t.Errorf("path %s", r.URL.Path)
		}
		json.NewDecoder(r.Body).Decode(&got)
		fmt.Fprint(w, `{"jobComplete": true, "rows": [
			{"f": [{"v": "www"}, {"v": "0.25"}]},
			{"f": [{"v": "_runner"}, {"v": "1.5"}]},
			{"f": [{"v": null}, {"v": "9"}]}]}`)
	}))
	defer srv.Close()
	e := &Export{API: srv.URL, Project: "billing-proj", Table: "kirsch-org.billing_export.gcp_billing_export_v1_*", Label: "nick-org", Client: srv.Client()}
	cost, err := e.Cost(context.Background(), time.Date(2026, 11, 19, 0, 0, 0, 0, Pacific))
	if err != nil {
		t.Fatal(err)
	}
	if len(cost) != 2 || cost["www"] != 0.25 || cost[RunnerValue] != 1.5 {
		t.Fatalf("cost = %v", cost)
	}
	q, _ := got["query"].(string)
	if !strings.Contains(q, "FROM `kirsch-org.billing_export.gcp_billing_export_v1_*`") {
		t.Errorf("query %q", q)
	}
	b, _ := json.Marshal(got["queryParameters"])
	if !strings.Contains(string(b), `"202611"`) || !strings.Contains(string(b), `"nick-org"`) {
		t.Errorf("params %s", b)
	}

	for _, bad := range []Export{
		{Project: "p", Table: "a.b.c`; DROP", Label: "x"},
		{Project: "p", Table: "a.b.c", Label: "Bad Label"},
		{Table: "a.b.c", Label: "x"},
	} {
		if bad.Validate() == nil {
			t.Errorf("%+v validated", bad)
		}
	}
}

func TestStripe(t *testing.T) {
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, _, _ := r.BasicAuth(); u != "sk_test" {
			t.Errorf("auth %q", u)
		}
		r.ParseForm()
		calls = append(calls, r.URL.Path+" "+r.Header.Get("Idempotency-Key")+" "+r.PostForm.Encode())
		switch {
		case r.URL.Path == "/v1/customers":
			fmt.Fprint(w, `{"id": "cus_1"}`)
		case r.URL.Path == "/v1/invoices":
			fmt.Fprint(w, `{"id": "in_1"}`)
		case r.URL.Path == "/v1/invoiceitems":
			if r.PostForm.Get("amount") == "0" {
				w.WriteHeader(400)
				fmt.Fprint(w, `{"error": {"message": "amount must be positive"}}`)
				return
			}
			fmt.Fprint(w, `{"id": "ii_1"}`)
		default:
			fmt.Fprint(w, `{"id": "in_1", "hosted_invoice_url": "https://pay.example/in_1"}`)
		}
	}))
	defer srv.Close()
	s := &Stripe{API: srv.URL, Key: "sk_test", Client: srv.Client()}
	ctx := context.Background()
	cus, err := s.Customer(ctx, "k1", "Nick", "nick@example.org")
	if err != nil || cus != "cus_1" {
		t.Fatal(cus, err)
	}
	inv, err := s.Draft(ctx, "k2", cus, "hosting", 14)
	if err != nil || inv != "in_1" {
		t.Fatal(inv, err)
	}
	if err := s.AddLine(ctx, "k3", cus, inv, Line{"www · AI", 136}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddLine(ctx, "k4", cus, inv, Line{"zero", 0}); err == nil || !strings.Contains(err.Error(), "amount must be positive") {
		t.Fatalf("zero line: %v", err)
	}
	sent, err := s.Send(ctx, inv)
	if err != nil || sent.URL != "https://pay.example/in_1" {
		t.Fatal(sent, err)
	}
	want := []string{
		"/v1/customers k1 " + url.Values{"email": {"nick@example.org"}, "name": {"Nick"}}.Encode(),
		"/v1/invoices k2 auto_advance=false&collection_method=send_invoice&currency=usd&customer=cus_1&days_until_due=14&description=hosting&pending_invoice_items_behavior=exclude",
		"/v1/invoiceitems k3 amount=136&currency=usd&customer=cus_1&description=www+%C2%B7+AI&invoice=in_1",
	}
	for i, w := range want {
		if calls[i] != w {
			t.Errorf("call %d = %q\nwant %q", i, calls[i], w)
		}
	}
	if !strings.HasPrefix(calls[4], "/v1/invoices/in_1/finalize finalize-in_1") || !strings.HasPrefix(calls[5], "/v1/invoices/in_1/send send-in_1") {
		t.Errorf("send calls %q", calls[4:])
	}
}

func sign(body, secret string, at time.Time) string {
	ts := fmt.Sprint(at.Unix())
	mac := hmac.New(sha256.New, []byte(secret))
	io.WriteString(mac, ts+"."+body)
	return "t=" + ts + ",v1=" + hex.EncodeToString(mac.Sum(nil))
}

func TestParseWebhook(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	body := `{"type": "invoice.paid", "data": {"object": {"id": "in_1", "object": "invoice"}}}`
	ev, err := ParseWebhook([]byte(body), sign(body, "whsec", now), "whsec", now)
	if err != nil || ev != (Event{Type: "invoice.paid", Invoice: "in_1"}) {
		t.Fatal(ev, err)
	}
	if InvoiceStatus(ev.Type) != "paid" || InvoiceStatus("invoice.created") != "" {
		t.Error("InvoiceStatus")
	}
	for name, c := range map[string]struct{ header, secret string }{
		"wrong secret": {sign(body, "other", now), "whsec"},
		"too old":      {sign(body, "whsec", now.Add(-10*time.Minute)), "whsec"},
		"malformed":    {"v1=abc", "whsec"},
		"tampered":     {sign(body+" ", "whsec", now), "whsec"},
	} {
		if _, err := ParseWebhook([]byte(body), c.header, c.secret, now); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
