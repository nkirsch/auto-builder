package db

import (
	"context"
	"time"
)

// Activity is the newest thing to happen on a board, as a kind and when.
type Activity struct {
	Kind string    `json:"kind"` // item, reply, run, new user or login
	At   time.Time `json:"at"`
}

// LastActivity is the board's newest activity of any kind and its newest by a
// person; either is nil if there is none. Runs and bot items and replies count
// only for the first. Logins come from members.last_seen, refreshed daily, and
// sign-ins, which on a hub live in the registry (Registry.LastSignIn).
func (s *Store) LastActivity(ctx context.Context) (all, human *Activity, err error) {
	// A human reply sets its item's human_activity_at to the same now(), which
	// is how it's told from a bot's; on a tie the reply names it, not the item.
	rows, err := s.Pool.Query(ctx, `SELECT kind, at, human FROM (
			SELECT 'item' AS kind, max(greatest(created_at, human_activity_at)) AS at, false AS human FROM items
			UNION ALL SELECT 'item', max(human_activity_at), true FROM items WHERE human_activity_at > 'epoch'
			UNION ALL SELECT 'reply', max(created_at), false FROM responses
			UNION ALL SELECT 'reply', max(r.created_at), true FROM responses r
				JOIN items i ON i.id = r.item_id WHERE r.created_at = i.human_activity_at
			UNION ALL SELECT 'run', max(greatest(created_at, finished_at)), false FROM runs
			UNION ALL SELECT 'new user', max(added_at), true FROM members
			UNION ALL SELECT 'login', max(last_seen), true FROM members
			UNION ALL SELECT 'login', max(at), true FROM signins WHERE result = $1
		) events WHERE at IS NOT NULL ORDER BY at DESC, kind = 'item'`, SignInOK)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var a Activity
		var isHuman bool
		if err := rows.Scan(&a.Kind, &a.At, &isHuman); err != nil {
			return nil, nil, err
		}
		if all == nil {
			all = &a
		}
		if human == nil && isHuman {
			human = &a
		}
	}
	return all, human, rows.Err()
}
