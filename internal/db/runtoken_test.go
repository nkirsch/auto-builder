package db

import (
	"context"
	"errors"
	"testing"
)

func TestTakeRunToken(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	id, err := s.CreateRun(ctx, "manual", "nkirsch")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.TakeRunToken(ctx, id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("requested: %v", err)
	}
	s.ClaimRun(ctx, id, "", "")
	if err := s.TakeRunToken(ctx, id); err != nil {
		t.Fatalf("running: %v", err)
	}
	if err := s.TakeRunToken(ctx, id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second token: %v", err)
	}
	if err := s.TakeRunToken(ctx, id+1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing run: %v", err)
	}
}
