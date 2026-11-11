package db

import (
	"context"
	"time"
)

// Seen is an IP + browser a signed-in session used.
type Seen struct {
	Identity  string
	IP        string
	UserAgent string
	First     time.Time
	Last      time.Time
}

// RecordSeen notes that identity's session made a request from ip + ua.
func RecordSeen(ctx context.Context, pool DB, identity, ip, ua string) error {
	_, err := pool.Exec(ctx, `INSERT INTO seen (identity, ip, user_agent) VALUES (lower($1), $2, $3)
		ON CONFLICT (identity, ip, user_agent) DO UPDATE SET last_at = now()`,
		identity, trim(ip, 64), trim(ua, 300))
	return err
}

// AllSeen lists the IPs + browsers sessions used since, most recent first.
func AllSeen(ctx context.Context, pool DB, since time.Time) ([]Seen, error) {
	rows, err := pool.Query(ctx, `SELECT identity, ip, user_agent, first_at, last_at FROM seen
		WHERE last_at > $1 ORDER BY last_at DESC`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Seen
	for rows.Next() {
		var s Seen
		if err := rows.Scan(&s.Identity, &s.IP, &s.UserAgent, &s.First, &s.Last); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// SeenIPs lists the IPs identities' sessions used since.
func SeenIPs(ctx context.Context, pool DB, identities []string, since time.Time) ([]string, error) {
	rows, err := pool.Query(ctx, `SELECT DISTINCT s.ip FROM seen s, unnest($1::text[]) id
		WHERE s.last_at > $2 AND s.ip <> '' AND s.identity = lower(id)`, identities, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var ip string
		if err := rows.Scan(&ip); err != nil {
			return nil, err
		}
		out = append(out, ip)
	}
	return out, rows.Err()
}

// PruneSeen drops what no session has used since before.
func PruneSeen(ctx context.Context, pool DB, before time.Time) error {
	_, err := pool.Exec(ctx, `DELETE FROM seen WHERE last_at < $1`, before)
	return err
}
