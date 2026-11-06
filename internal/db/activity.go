package db

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// Activity is the newest thing to happen on a board, as a kind and when.
type Activity struct {
	Kind string    `json:"kind"` // item, reply, run, new user or login
	At   time.Time `json:"at"`
}

// LastActivity is the board's newest activity, nil if it has none. Logins come
// from members.last_seen, refreshed daily, and sign-ins, which on a hub live
// in the registry (Registry.LastSignIn). A reply also bumps its item's
// human_activity_at, so on a tie the item loses.
func (s *Store) LastActivity(ctx context.Context) (*Activity, error) {
	var a Activity
	err := s.Pool.QueryRow(ctx, `SELECT kind, at FROM (
			SELECT 'item' AS kind, max(greatest(created_at, human_activity_at)) AS at FROM items
			UNION ALL SELECT 'reply', max(created_at) FROM responses
			UNION ALL SELECT 'run', max(greatest(created_at, finished_at)) FROM runs
			UNION ALL SELECT 'new user', max(added_at) FROM members
			UNION ALL SELECT 'login', max(last_seen) FROM members
			UNION ALL SELECT 'login', max(at) FROM signins WHERE result = $1
		) events WHERE at IS NOT NULL ORDER BY at DESC, kind = 'item' LIMIT 1`, SignInOK).Scan(&a.Kind, &a.At)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &a, nil
}
