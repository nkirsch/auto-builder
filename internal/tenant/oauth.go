package tenant

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/nkirsch/auto-builder/internal/db"
)

const (
	// OAuthAccessPrefix marks a hub access token, so /mcp can never mistake one
	// for a run token (bare hex) or the reverse.
	OAuthAccessPrefix  = "hat_"
	oauthRefreshPrefix = "hrt_"

	OAuthAccessTTL  = time.Hour
	oauthRefreshTTL = 30 * 24 * time.Hour
	oauthCodeTTL    = 5 * time.Minute

	// Clients may refresh concurrently; a spent token only ends the grant once
	// it's reused after this long.
	oauthReuseGrace = time.Minute
	// A registered client that never got a token is dropped after this long.
	oauthStaleClient = 24 * time.Hour
	oauthMaxClients  = 10000
)

// ErrOAuthFull means too many clients are registered to take another.
var ErrOAuthFull = errors.New("too many OAuth clients")

// OAuthClient is a registered client.
type OAuthClient struct {
	ID           string
	Name         string
	RedirectURIs []string
}

// OAuthCode is what an authorization code was issued for.
type OAuthCode struct {
	ClientID, Identity, RedirectURI, Challenge string
}

// OAuthTokens is one token response.
type OAuthTokens struct {
	Access, Refresh string
}

func randomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// RegisterOAuthClient stores a client, clearing out abandoned ones first:
// registration is open, so it mustn't grow without bound.
func (r *Registry) RegisterOAuthClient(ctx context.Context, name string, redirectURIs []string) (string, error) {
	if _, err := r.Control.Exec(ctx, `DELETE FROM oauth_clients c WHERE created_at < $1
		AND NOT EXISTS (SELECT 1 FROM oauth_tokens t WHERE t.client_id = c.id)`, time.Now().Add(-oauthStaleClient)); err != nil {
		return "", err
	}
	var n int
	if err := r.Control.QueryRow(ctx, `SELECT count(*) FROM oauth_clients`).Scan(&n); err != nil {
		return "", err
	}
	if n >= oauthMaxClients {
		return "", ErrOAuthFull
	}
	id, err := randomToken()
	if err != nil {
		return "", err
	}
	id = "hc_" + id[:32]
	_, err = r.Control.Exec(ctx, `INSERT INTO oauth_clients (id, name, redirect_uris) VALUES ($1, $2, $3)`, id, name, redirectURIs)
	return id, err
}

// OAuthClientByID returns a client, or db.ErrNotFound.
func (r *Registry) OAuthClientByID(ctx context.Context, id string) (OAuthClient, error) {
	c := OAuthClient{ID: id}
	err := r.Control.QueryRow(ctx, `SELECT name, redirect_uris FROM oauth_clients WHERE id = $1`, id).Scan(&c.Name, &c.RedirectURIs)
	if errors.Is(err, pgx.ErrNoRows) {
		err = db.ErrNotFound
	}
	return c, err
}

// IssueOAuthCode mints a short-lived authorization code.
func (r *Registry) IssueOAuthCode(ctx context.Context, c OAuthCode) (string, error) {
	code, err := randomToken()
	if err != nil {
		return "", err
	}
	_, err = r.Control.Exec(ctx, `INSERT INTO oauth_codes (hash, client_id, identity, redirect_uri, challenge, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6)`, hashToken(code), c.ClientID, c.Identity, c.RedirectURI, c.Challenge, time.Now().Add(oauthCodeTTL))
	return code, err
}

// TakeOAuthCode spends a code: it works once, even if the exchange then fails.
func (r *Registry) TakeOAuthCode(ctx context.Context, code string) (OAuthCode, error) {
	var c OAuthCode
	err := r.Control.QueryRow(ctx, `DELETE FROM oauth_codes WHERE hash = $1 AND expires_at > now()
		RETURNING client_id, identity, redirect_uri, challenge`, hashToken(code)).Scan(&c.ClientID, &c.Identity, &c.RedirectURI, &c.Challenge)
	if errors.Is(err, pgx.ErrNoRows) {
		err = db.ErrNotFound
	}
	return c, err
}

// IssueOAuthTokens starts a grant for identity.
func (r *Registry) IssueOAuthTokens(ctx context.Context, clientID, identity string) (OAuthTokens, error) {
	if _, err := r.Control.Exec(ctx, `DELETE FROM oauth_tokens WHERE expires_at < now()`); err != nil {
		return OAuthTokens{}, err
	}
	if _, err := r.Control.Exec(ctx, `DELETE FROM oauth_codes WHERE expires_at < now()`); err != nil {
		return OAuthTokens{}, err
	}
	grant, err := randomToken()
	if err != nil {
		return OAuthTokens{}, err
	}
	return r.oauthTokens(ctx, grant, clientID, identity)
}

func (r *Registry) oauthTokens(ctx context.Context, grant, clientID, identity string) (OAuthTokens, error) {
	access, err := randomToken()
	if err != nil {
		return OAuthTokens{}, err
	}
	refresh, err := randomToken()
	if err != nil {
		return OAuthTokens{}, err
	}
	t := OAuthTokens{Access: OAuthAccessPrefix + access, Refresh: oauthRefreshPrefix + refresh}
	now := time.Now()
	_, err = r.Control.Exec(ctx, `INSERT INTO oauth_tokens (hash, kind, grant_id, client_id, identity, expires_at)
		VALUES ($1, 'access', $3, $4, $5, $6), ($2, 'refresh', $3, $4, $5, $7)`,
		hashToken(t.Access), hashToken(t.Refresh), grant, clientID, identity, now.Add(OAuthAccessTTL), now.Add(oauthRefreshTTL))
	return t, err
}

// RefreshOAuthTokens spends a refresh token for new tokens in the same grant.
// Reusing a spent one past the grace period ends the grant: one of its two
// holders has stolen it. The client must match, or db.ErrNotFound.
func (r *Registry) RefreshOAuthTokens(ctx context.Context, clientID, refresh string) (OAuthTokens, error) {
	var grant, identity string
	err := r.Control.QueryRow(ctx, `UPDATE oauth_tokens SET used_at = now()
		WHERE hash = $1 AND kind = 'refresh' AND client_id = $2 AND used_at IS NULL AND expires_at > now()
		RETURNING grant_id, identity`, hashToken(refresh), clientID).Scan(&grant, &identity)
	if errors.Is(err, pgx.ErrNoRows) {
		if _, err := r.Control.Exec(ctx, `DELETE FROM oauth_tokens WHERE grant_id IN (SELECT grant_id FROM oauth_tokens
			WHERE hash = $1 AND kind = 'refresh' AND used_at < $2)`, hashToken(refresh), time.Now().Add(-oauthReuseGrace)); err != nil {
			return OAuthTokens{}, err
		}
		return OAuthTokens{}, db.ErrNotFound
	}
	if err != nil {
		return OAuthTokens{}, err
	}
	return r.oauthTokens(ctx, grant, clientID, identity)
}

// OAuthAccess resolves an unexpired access token to its identity, or db.ErrNotFound.
func (r *Registry) OAuthAccess(ctx context.Context, token string) (string, error) {
	var identity string
	err := r.Control.QueryRow(ctx, `SELECT identity FROM oauth_tokens
		WHERE hash = $1 AND kind = 'access' AND expires_at > now()`, hashToken(token)).Scan(&identity)
	if errors.Is(err, pgx.ErrNoRows) {
		err = db.ErrNotFound
	}
	return identity, err
}
