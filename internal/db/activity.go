package db

import (
	"context"
	"strings"
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
//
// exclude names logins and emails (a hub's platform admins) whose activity
// isn't the board's people's: with it, the newest by a person counts only
// items and replies by the other members, since an item's human_activity_at
// doesn't say who bumped it.
func (s *Store) LastActivity(ctx context.Context, exclude ...string) (all, human *Activity, err error) {
	ex := lowered(exclude)
	// A human reply sets its item's human_activity_at to the same now(), which
	// is how it's told from a bot's; on a tie the reply names it, not the item.
	rows, err := s.Pool.Query(ctx, `WITH people AS (SELECT login, added_at, last_seen FROM members
			WHERE lower(login) <> ALL($2) AND lower(email) <> ALL($2))
		SELECT kind, at, human FROM (
			SELECT 'item' AS kind, max(greatest(created_at, human_activity_at)) AS at, false AS human FROM items
			UNION ALL SELECT 'item', max(CASE WHEN $3 THEN created_at ELSE human_activity_at END), true FROM items
				WHERE human_activity_at > 'epoch' AND (NOT $3 OR lower(author) IN (SELECT lower(login) FROM people))
			UNION ALL SELECT 'reply', max(created_at), false FROM responses
			UNION ALL SELECT 'reply', max(r.created_at), true FROM responses r
				JOIN items i ON i.id = r.item_id WHERE CASE WHEN $3
					THEN lower(r.author) IN (SELECT lower(login) FROM people)
					ELSE r.created_at = i.human_activity_at END
			UNION ALL SELECT 'run', max(greatest(created_at, finished_at)), false FROM runs
			UNION ALL SELECT 'new user', max(added_at), true FROM people
			UNION ALL SELECT 'login', max(last_seen), true FROM people
			UNION ALL SELECT 'login', max(at), true FROM signins WHERE result = $1 AND lower(identity) <> ALL($2)
		) events WHERE at IS NOT NULL ORDER BY at DESC, kind = 'item'`, SignInOK, ex, len(ex) > 0)
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

// lowered is ids lowercased, without empty ones.
func lowered(ids []string) []string {
	out := []string{}
	for _, id := range ids {
		if id != "" {
			out = append(out, strings.ToLower(id))
		}
	}
	return out
}
