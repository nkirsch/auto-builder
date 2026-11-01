package db

import (
	"context"
	"testing"
)

func TestPaymentsRaiseSpendLimit(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	if added, err := s.RecordPayment(ctx, "cs_1", "ann", 15); err != nil || !added {
		t.Fatalf("record = %v, %v", added, err)
	}
	if added, err := s.RecordPayment(ctx, "cs_1", "ann", 15); err != nil || added {
		t.Fatalf("second record = %v, %v; want not added", added, err)
	}
	// a payment older than 30 days no longer counts
	if _, err := s.Pool.Exec(ctx, `INSERT INTO payments (id, login, amount_usd, created_at) VALUES ('cs_old', 'bo', 99, now() - interval '31 days')`); err != nil {
		t.Fatal(err)
	}
	if c, err := s.Credit30(ctx); err != nil || c != 15 {
		t.Fatalf("credit = %v, %v; want 15", c, err)
	}
	g, err := s.GateState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if g.SpendLimit != 35 {
		t.Errorf("gate limit = %v, want 20 + 15", g.SpendLimit)
	}
	ps, err := s.Payments(ctx, 10)
	if err != nil || len(ps) != 2 || ps[0].ID != "cs_1" || ps[0].USD != 15 {
		t.Errorf("payments = %+v, %v", ps, err)
	}
	if err := s.SetPaymentsOn(ctx, true); err != nil {
		t.Fatal(err)
	}
	if on, err := s.PaymentsOn(ctx); err != nil || !on {
		t.Errorf("payments on = %v, %v", on, err)
	}
	s.SetPaymentsOn(ctx, false)
}
