package db

import (
	"context"
	"os"
	"testing"
)

// TEST_DATABASE_URL must point at a disposable database; tests skip without it.
func testURL(t *testing.T) string {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	return url
}

func TestMigrateIdempotent(t *testing.T) {
	ctx := context.Background()
	pool, err := Open(ctx, testURL(t))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM schema_migrations WHERE version = '001_init'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("001_init recorded %d times", n)
	}
}

func TestOneActiveRun(t *testing.T) {
	ctx := context.Background()
	pool, err := Open(ctx, testURL(t))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `INSERT INTO runs (trigger) VALUES ('test')`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO runs (trigger) VALUES ('test')`); err == nil {
		t.Fatal("second active run inserted; runs_one_active not enforced")
	}
}
