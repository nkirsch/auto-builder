package tenant

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/nkirsch/auto-builder/internal/db"
)

// ssoCodeTTL bounds the browser's trip from the hub back to the app.
const ssoCodeTTL = 2 * time.Minute

// SSOCode is what an app sign-in code was issued for.
type SSOCode struct {
	Client, Identity, RedirectURI, Challenge, Nonce string
}

// IssueSSOCode mints a one-time code for an app, dropping expired ones.
func (r *Registry) IssueSSOCode(ctx context.Context, c SSOCode) (string, error) {
	if _, err := r.Control.Exec(ctx, `DELETE FROM sso_codes WHERE expires_at < now()`); err != nil {
		return "", err
	}
	code, err := randomToken()
	if err != nil {
		return "", err
	}
	_, err = r.Control.Exec(ctx, `INSERT INTO sso_codes (hash, client, identity, redirect_uri, challenge, nonce, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`, hashToken(code), c.Client, c.Identity, c.RedirectURI, c.Challenge, c.Nonce,
		time.Now().Add(ssoCodeTTL))
	return code, err
}

// TakeSSOCode spends a code: it works once, even if the exchange then fails.
func (r *Registry) TakeSSOCode(ctx context.Context, code string) (SSOCode, error) {
	var c SSOCode
	err := r.Control.QueryRow(ctx, `DELETE FROM sso_codes WHERE hash = $1 AND expires_at > now()
		RETURNING client, identity, redirect_uri, challenge, nonce`, hashToken(code)).
		Scan(&c.Client, &c.Identity, &c.RedirectURI, &c.Challenge, &c.Nonce)
	if errors.Is(err, pgx.ErrNoRows) {
		err = db.ErrNotFound
	}
	return c, err
}
