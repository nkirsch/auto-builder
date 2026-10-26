package tenant

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/nkirsch/auto-builder/internal/db"
)

// RunTokenTTL bounds a run token; the runner's agent stops at 55 minutes.
const RunTokenTTL = 70 * time.Minute

func hashToken(token string) []byte {
	h := sha256.Sum256([]byte(token))
	return h[:]
}

// IssueRunToken mints the RUN_TOKEN for one tenant's run, storing only its hash.
func (r *Registry) IssueRunToken(ctx context.Context, slug string, runID int) (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	token := hex.EncodeToString(b)
	_, err := r.Control.Exec(ctx, `INSERT INTO run_tokens (hash, tenant, run_id, expires_at) VALUES ($1, $2, $3, $4)`,
		hashToken(token), slug, runID, time.Now().Add(RunTokenTTL))
	return token, err
}

// RunToken resolves an unexpired token to its tenant and run, or db.ErrNotFound.
func (r *Registry) RunToken(ctx context.Context, token string) (slug string, runID int, err error) {
	err = r.Control.QueryRow(ctx, `SELECT tenant, run_id FROM run_tokens WHERE hash = $1 AND expires_at > now()`,
		hashToken(token)).Scan(&slug, &runID)
	if errors.Is(err, pgx.ErrNoRows) {
		err = db.ErrNotFound
	}
	return slug, runID, err
}

// RevokeRunToken ends a run's token once the run finishes.
func (r *Registry) RevokeRunToken(ctx context.Context, slug string, runID int) error {
	_, err := r.Control.Exec(ctx, `DELETE FROM run_tokens WHERE tenant = $1 AND run_id = $2`, slug, runID)
	return err
}

// ActiveRuns counts runs holding an unexpired token, across all tenants.
func (r *Registry) ActiveRuns(ctx context.Context) (int, error) {
	var n int
	err := r.Control.QueryRow(ctx, `SELECT count(*) FROM run_tokens WHERE expires_at > now()`).Scan(&n)
	return n, err
}
