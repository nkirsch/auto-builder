package db

import (
	"context"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// SignInKeep is how long sign-in attempts are kept.
const SignInKeep = 90 * 24 * time.Hour

// SignIn is one sign-in attempt.
type SignIn struct {
	At        time.Time
	Provider  string // github or google
	Identity  string // GitHub login or lower-cased address; "" when the provider never said
	Result    string // SignInOK or why it failed
	Board     string // multi-tenant mode: the tenant's slug; "" when unknown or on a single board
	IP        string
	UserAgent string
}

// Sign-in results.
const (
	SignInOK          = "ok"
	SignInNotMember   = "not a member"
	SignInGoogleOnly  = "Google only"
	SignInUnverified  = "unverified email"
	SignInExchange    = "exchange failed"
	SignInProfile     = "profile failed"
	SignInStateBad    = "state mismatch"
	SignInStateMissed = "expired"
)

// Failed reports whether the attempt didn't sign anyone in.
func (s SignIn) Failed() bool { return s.Result != SignInOK }

// SignInLog records sign-in attempts and lists them for admins: a Store on a
// single board, the hub's control schema in multi-tenant mode.
type SignInLog interface {
	// RecordSignIn adds s, unless within > 0 and the same attempt (provider,
	// identity, result, board) was recorded that recently.
	RecordSignIn(ctx context.Context, s SignIn, within time.Duration) error
	// SignIns lists attempts since, newest first: board's, plus failures
	// anywhere by identities; with only, just identities' attempts.
	SignIns(ctx context.Context, board string, identities []string, only bool, since time.Time) ([]SignIn, error)
	PruneSignIns(ctx context.Context, before time.Time) error
}

func (s *Store) RecordSignIn(ctx context.Context, in SignIn, within time.Duration) error {
	return RecordSignIn(ctx, s.Pool, in, within)
}

func (s *Store) SignIns(ctx context.Context, board string, identities []string, only bool, since time.Time) ([]SignIn, error) {
	return SignIns(ctx, s.Pool, board, identities, only, since)
}

func (s *Store) PruneSignIns(ctx context.Context, before time.Time) error {
	return PruneSignIns(ctx, s.Pool, before)
}

// RecordSignIn, SignIns and PruneSignIns implement SignInLog on pool's
// signins table, for the board's and the control schema's alike.
func RecordSignIn(ctx context.Context, pool *pgxpool.Pool, s SignIn, within time.Duration) error {
	_, err := pool.Exec(ctx, `INSERT INTO signins (provider, identity, result, board, ip, user_agent)
		SELECT $1, $2, $3, $4, $5, $6 WHERE $7 <= 0 OR NOT EXISTS (SELECT 1 FROM signins
			WHERE provider = $1 AND lower(identity) = lower($2) AND result = $3 AND board = $4
			AND at > now() - make_interval(secs => $7))`,
		s.Provider, s.Identity, s.Result, s.Board, trim(s.IP, 64), trim(s.UserAgent, 300), within.Seconds())
	return err
}

func SignIns(ctx context.Context, pool *pgxpool.Pool, board string, identities []string, only bool, since time.Time) ([]SignIn, error) {
	ids := make([]string, len(identities))
	for i, id := range identities {
		ids[i] = strings.ToLower(id)
	}
	rows, err := pool.Query(ctx, `SELECT at, provider, identity, result, board, ip, user_agent FROM signins
		WHERE at > $4 AND (NOT $3 OR lower(identity) = ANY($2))
		AND (board = $1 OR (result <> 'ok' AND lower(identity) = ANY($2)))
		ORDER BY at DESC, id DESC LIMIT 500`, board, ids, only, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SignIn
	for rows.Next() {
		var s SignIn
		if err := rows.Scan(&s.At, &s.Provider, &s.Identity, &s.Result, &s.Board, &s.IP, &s.UserAgent); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// AllSignIns lists attempts since, newest first, at every board or, with
// board set, one ("-" for none); identity, when set, narrows to it.
func AllSignIns(ctx context.Context, pool *pgxpool.Pool, board, identity string, since time.Time) ([]SignIn, error) {
	if board == "-" {
		board = ""
	} else if board == "" {
		board = "*"
	}
	rows, err := pool.Query(ctx, `SELECT at, provider, identity, result, board, ip, user_agent FROM signins
		WHERE at > $3 AND ($1 = '*' OR board = $1) AND ($2 = '' OR lower(identity) = lower($2))
		ORDER BY at DESC, id DESC LIMIT 1000`, board, identity, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SignIn
	for rows.Next() {
		var s SignIn
		if err := rows.Scan(&s.At, &s.Provider, &s.Identity, &s.Result, &s.Board, &s.IP, &s.UserAgent); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func PruneSignIns(ctx context.Context, pool *pgxpool.Pool, before time.Time) error {
	_, err := pool.Exec(ctx, `DELETE FROM signins WHERE at < $1`, before)
	return err
}

func trim(s string, n int) string {
	if len(s) > n {
		return strings.ToValidUTF8(s[:n], "")
	}
	return s
}
