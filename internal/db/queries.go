package db

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNotFound is returned when a row doesn't exist.
var ErrNotFound = errors.New("not found")

// ErrRunActive is returned by CreateRun when a run is already in flight.
var ErrRunActive = errors.New("a run is already active")

// Store wraps the pool with the app's queries.
type Store struct{ Pool *pgxpool.Pool }

type Member struct {
	Login     string
	Role      string
	Name      string
	AvatarURL string
	AddedBy   string
	AddedAt   time.Time
	LastSeen  *time.Time
}

func (m Member) IsAdmin() bool { return m.Role == "admin" }

type Item struct {
	ID              int
	Title           string
	Body            string
	Author          string
	Status          string
	Approved        bool
	ApprovedBy      string
	PRNumber        *int
	MergeSHA        string
	CloseNote       string
	CreatedAt       time.Time
	UpdatedAt       time.Time
	HumanActivityAt time.Time
	ClosedAt        *time.Time
	Replies         int // list queries only
}

type Response struct {
	ID        int
	ItemID    int
	Author    string
	Body      string
	CreatedAt time.Time
}

type Attachment struct {
	ID          int
	ItemID      int
	Name        string
	ContentType string
	Size        int64
	Object      string
	UploadedBy  string
	CreatedAt   time.Time
}

type Notification struct {
	ID        int
	Login     string
	ItemID    int
	ItemTitle string
	Actor     string
	Kind      string
	ReadAt    *time.Time
	CreatedAt time.Time
	Count     int // notifications on this item, when listed per item
}

type Run struct {
	ID          int
	Status      string
	Trigger     string
	RequestedBy string
	Execution   string
	Summary     string
	LogObject   string
	CreatedAt   time.Time
	StartedAt   *time.Time
	FinishedAt  *time.Time
}

func notFound(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

// Members

const memberCols = `login, role, name, avatar_url, added_by, added_at, last_seen`

func scanMember(row pgx.Row) (Member, error) {
	var m Member
	err := row.Scan(&m.Login, &m.Role, &m.Name, &m.AvatarURL, &m.AddedBy, &m.AddedAt, &m.LastSeen)
	return m, notFound(err)
}

// Member looks up a login case-insensitively (GitHub logins are).
func (s *Store) Member(ctx context.Context, login string) (Member, error) {
	return scanMember(s.Pool.QueryRow(ctx, `SELECT `+memberCols+` FROM members WHERE lower(login) = lower($1)`, login))
}

func (s *Store) Members(ctx context.Context) ([]Member, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+memberCols+` FROM members ORDER BY role, login`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Member, error) { return scanMember(r) })
}

// UpsertMember adds a member or changes their role.
func (s *Store) UpsertMember(ctx context.Context, login, role, addedBy string) error {
	_, err := s.Pool.Exec(ctx, `INSERT INTO members (login, role, added_by) VALUES ($1, $2, $3)
		ON CONFLICT (login) DO UPDATE SET role = EXCLUDED.role`, login, role, addedBy)
	return err
}

func (s *Store) DeleteMember(ctx context.Context, login string) error {
	_, err := s.Pool.Exec(ctx, `DELETE FROM members WHERE lower(login) = lower($1)`, login)
	return err
}

// TouchMember records a sign-in with the GitHub profile fields.
func (s *Store) TouchMember(ctx context.Context, login, name, avatar string) error {
	_, err := s.Pool.Exec(ctx, `UPDATE members SET name = $2, avatar_url = $3, last_seen = now()
		WHERE lower(login) = lower($1)`, login, name, avatar)
	return err
}

// Items

const itemCols = `i.id, i.title, i.body, i.author, i.status, i.approved, i.approved_by, i.pr_number,
	i.merge_sha, i.close_note, i.created_at, i.updated_at, i.human_activity_at, i.closed_at`

func scanItem(row pgx.Row, extra ...any) (Item, error) {
	var it Item
	dest := []any{&it.ID, &it.Title, &it.Body, &it.Author, &it.Status, &it.Approved, &it.ApprovedBy,
		&it.PRNumber, &it.MergeSHA, &it.CloseNote, &it.CreatedAt, &it.UpdatedAt, &it.HumanActivityAt, &it.ClosedAt}
	err := row.Scan(append(dest, extra...)...)
	return it, notFound(err)
}

func (s *Store) Item(ctx context.Context, id int) (Item, error) {
	return scanItem(s.Pool.QueryRow(ctx, `SELECT `+itemCols+` FROM items i WHERE id = $1`, id))
}

// Items lists items in the given statuses (all when empty), newest activity first.
func (s *Store) Items(ctx context.Context, statuses ...string) ([]Item, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+itemCols+`, (SELECT count(*) FROM responses r WHERE r.item_id = i.id)
		FROM items i WHERE cardinality($1::text[]) = 0 OR i.status = ANY($1)
		ORDER BY i.updated_at DESC`, statuses)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Item, error) {
		var it Item
		it, err := scanItem(r, &it.Replies)
		return it, err
	})
}

func (s *Store) CreateItem(ctx context.Context, title, body, author string) (int, error) {
	var id int
	err := s.Pool.QueryRow(ctx, `INSERT INTO items (title, body, author) VALUES ($1, $2, $3) RETURNING id`,
		title, body, author).Scan(&id)
	return id, err
}

// CreateBotItem files an item without counting as human activity.
func (s *Store) CreateBotItem(ctx context.Context, title, body, author string) (int, error) {
	var id int
	err := s.Pool.QueryRow(ctx, `INSERT INTO items (title, body, author, human_activity_at)
		VALUES ($1, $2, $3, '-infinity') RETURNING id`, title, body, author).Scan(&id)
	return id, err
}

// SetApproved flips the approval flag; approving is human activity.
func (s *Store) SetApproved(ctx context.Context, id int, approved bool, by string) error {
	if !approved {
		by = ""
	}
	return s.execOne(ctx, `UPDATE items SET approved = $2, approved_by = $3, updated_at = now(),
		human_activity_at = now() WHERE id = $1`, id, approved, by)
}

// CloseItem closes an item with a note; human marks it as human activity.
func (s *Store) CloseItem(ctx context.Context, id int, note string, human bool) error {
	return s.execOne(ctx, `UPDATE items SET status = 'closed', close_note = $2, closed_at = now(), updated_at = now(),
		human_activity_at = CASE WHEN $3 THEN now() ELSE human_activity_at END WHERE id = $1`, id, note, human)
}

func (s *Store) ReopenItem(ctx context.Context, id int) error {
	return s.execOne(ctx, `UPDATE items SET status = 'open', close_note = '', closed_at = NULL, pr_number = NULL,
		merge_sha = '', updated_at = now(), human_activity_at = now() WHERE id = $1`, id)
}

// SetPR marks an item as building on a PR.
func (s *Store) SetPR(ctx context.Context, id, pr int) error {
	return s.execOne(ctx, `UPDATE items SET status = 'building', pr_number = $2, updated_at = now()
		WHERE id = $1 AND status IN ('open', 'building')`, id, pr)
}

func (s *Store) MarkMerged(ctx context.Context, id int, sha string) error {
	return s.execOne(ctx, `UPDATE items SET status = 'merged', merge_sha = $2, updated_at = now()
		WHERE id = $1 AND status = 'building'`, id, sha)
}

func (s *Store) MarkShipped(ctx context.Context, id int) error {
	return s.execOne(ctx, `UPDATE items SET status = 'shipped', updated_at = now() WHERE id = $1 AND status = 'merged'`, id)
}

// PRClosed returns a building item to open after its PR closed unmerged.
func (s *Store) PRClosed(ctx context.Context, id int) error {
	return s.execOne(ctx, `UPDATE items SET status = 'open', pr_number = NULL, updated_at = now()
		WHERE id = $1 AND status = 'building'`, id)
}

func (s *Store) execOne(ctx context.Context, sql string, args ...any) error {
	tag, err := s.Pool.Exec(ctx, sql, args...)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// Responses

func (s *Store) Responses(ctx context.Context, itemID int) ([]Response, error) {
	rows, err := s.Pool.Query(ctx, `SELECT id, item_id, author, body, created_at FROM responses
		WHERE item_id = $1 ORDER BY id`, itemID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[Response])
}

// AddResponse posts a reply; human replies bump the item's activity.
func (s *Store) AddResponse(ctx context.Context, itemID int, author, body string, human bool) (int, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	var id int
	if err := tx.QueryRow(ctx, `INSERT INTO responses (item_id, author, body) VALUES ($1, $2, $3) RETURNING id`,
		itemID, author, body).Scan(&id); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23503" {
			return 0, ErrNotFound
		}
		return 0, err
	}
	if _, err := tx.Exec(ctx, `UPDATE items SET updated_at = now(),
		human_activity_at = CASE WHEN $2 THEN now() ELSE human_activity_at END WHERE id = $1`, itemID, human); err != nil {
		return 0, err
	}
	return id, tx.Commit(ctx)
}

// Attachments

func (s *Store) AddAttachment(ctx context.Context, a Attachment) (int, error) {
	var id int
	err := s.Pool.QueryRow(ctx, `INSERT INTO attachments (item_id, name, content_type, size, object, uploaded_by)
		VALUES ($1, $2, $3, $4, $5, $6) RETURNING id`,
		a.ItemID, a.Name, a.ContentType, a.Size, a.Object, a.UploadedBy).Scan(&id)
	return id, err
}

func (s *Store) Attachments(ctx context.Context, itemID int) ([]Attachment, error) {
	rows, err := s.Pool.Query(ctx, `SELECT id, item_id, name, content_type, size, object, uploaded_by, created_at
		FROM attachments WHERE item_id = $1 ORDER BY id`, itemID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[Attachment])
}

func (s *Store) Attachment(ctx context.Context, id int) (Attachment, error) {
	var a Attachment
	err := s.Pool.QueryRow(ctx, `SELECT id, item_id, name, content_type, size, object, uploaded_by, created_at
		FROM attachments WHERE id = $1`, id).
		Scan(&a.ID, &a.ItemID, &a.Name, &a.ContentType, &a.Size, &a.Object, &a.UploadedBy, &a.CreatedAt)
	return a, notFound(err)
}

// Notifications

func (s *Store) Notify(ctx context.Context, login string, itemID int, actor, kind string) error {
	_, err := s.Pool.Exec(ctx, `INSERT INTO notifications (login, item_id, actor, kind) VALUES ($1, $2, $3, $4)`,
		login, itemID, actor, kind)
	return err
}

// Notifications lists the login's inbox, one row per item, newest activity
// first: the item's latest notification, with ReadAt nil while any of its
// notifications is unread and Count the number collapsed into the row. done
// selects fully read items that are shipped or closed; the rest are the inbox,
// so unread ones stay there until read.
func (s *Store) Notifications(ctx context.Context, login string, done bool, limit int) ([]Notification, error) {
	rows, err := s.Pool.Query(ctx, `SELECT l.id, l.login, l.item_id, i.title, l.actor, l.kind,
			CASE WHEN g.unread > 0 THEN NULL ELSE g.last_read END, l.created_at, g.n
		FROM (SELECT item_id, count(*) AS n, count(*) FILTER (WHERE read_at IS NULL) AS unread,
				max(read_at) AS last_read, max(id) AS last_id
			FROM notifications WHERE lower(login) = lower($1) GROUP BY item_id) g
		JOIN notifications l ON l.id = g.last_id
		JOIN items i ON i.id = g.item_id
		WHERE (g.unread = 0 AND i.status IN ('shipped', 'closed')) = $2
		ORDER BY l.id DESC LIMIT $3`, login, done, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[Notification])
}

// UnreadCount is the number of items with unread notifications for the login.
func (s *Store) UnreadCount(ctx context.Context, login string) (int, error) {
	var n int
	err := s.Pool.QueryRow(ctx, `SELECT count(DISTINCT item_id) FROM notifications WHERE lower(login) = lower($1) AND read_at IS NULL`,
		login).Scan(&n)
	return n, err
}

// MarkRead marks the login's notifications read, all of them when itemID is 0.
func (s *Store) MarkRead(ctx context.Context, login string, itemID int) error {
	_, err := s.Pool.Exec(ctx, `UPDATE notifications SET read_at = now()
		WHERE lower(login) = lower($1) AND read_at IS NULL AND ($2 = 0 OR item_id = $2)`, login, itemID)
	return err
}

// Runs

const runCols = `id, status, trigger, requested_by, execution, summary, log_object, created_at, started_at, finished_at`

func (s *Store) Run(ctx context.Context, id int) (Run, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+runCols+` FROM runs WHERE id = $1`, id)
	if err != nil {
		return Run{}, err
	}
	r, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByPos[Run])
	return r, notFound(err)
}

func (s *Store) Runs(ctx context.Context, limit int) ([]Run, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+runCols+` FROM runs ORDER BY id DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[Run])
}

// CreateRun inserts a requested run, or returns ErrRunActive.
func (s *Store) CreateRun(ctx context.Context, trigger, by string) (int, error) {
	var id int
	err := s.Pool.QueryRow(ctx, `INSERT INTO runs (trigger, requested_by) VALUES ($1, $2) RETURNING id`,
		trigger, by).Scan(&id)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return 0, ErrRunActive
	}
	return id, err
}

func (s *Store) SetExecution(ctx context.Context, id int, execution string) error {
	return s.execOne(ctx, `UPDATE runs SET execution = $2 WHERE id = $1`, id, execution)
}

// ClaimRun moves a requested run to running.
func (s *Store) ClaimRun(ctx context.Context, id int) error {
	return s.execOne(ctx, `UPDATE runs SET status = 'running', started_at = now() WHERE id = $1 AND status = 'requested'`, id)
}

// FinishRun records the outcome of an active run.
func (s *Store) FinishRun(ctx context.Context, id int, status, summary, logObject string) error {
	return s.execOne(ctx, `UPDATE runs SET status = $2, summary = $3, log_object = $4, finished_at = now(),
		started_at = coalesce(started_at, now()) WHERE id = $1 AND status IN ('requested', 'running')`,
		id, status, summary, logObject)
}

// FailStale fails runs stuck running longer than running, or never claimed within requested.
func (s *Store) FailStale(ctx context.Context, running, requested time.Duration) (int64, error) {
	tag, err := s.Pool.Exec(ctx, `UPDATE runs SET status = 'failed', finished_at = now(),
		summary = CASE WHEN status = 'running' THEN 'timed out' ELSE 'never started' END
		WHERE (status = 'running' AND started_at < now() - $1::interval)
		   OR (status = 'requested' AND created_at < now() - $2::interval)`, running, requested)
	return tag.RowsAffected(), err
}

// GateState is what the gate needs to decide whether to launch.
type GateState struct {
	Active       bool
	LastSuccess  *time.Time // started_at of the newest done run
	LastFailure  *time.Time // finished_at of the newest failed run
	NewActivity  bool       // an open/building item has human activity after LastSuccess
	ApprovedOpen bool       // an approved open item exists
}

func (s *Store) GateState(ctx context.Context) (GateState, error) {
	var g GateState
	err := s.Pool.QueryRow(ctx, `
		WITH last_ok AS (SELECT max(started_at) AS t FROM runs WHERE status = 'done')
		SELECT
			EXISTS (SELECT 1 FROM runs WHERE status IN ('requested', 'running')),
			(SELECT t FROM last_ok),
			(SELECT max(finished_at) FROM runs WHERE status = 'failed'),
			EXISTS (SELECT 1 FROM items WHERE status IN ('open', 'building')
				AND human_activity_at > coalesce((SELECT t FROM last_ok), '-infinity')),
			EXISTS (SELECT 1 FROM items WHERE status = 'open' AND approved)`).
		Scan(&g.Active, &g.LastSuccess, &g.LastFailure, &g.NewActivity, &g.ApprovedOpen)
	return g, err
}

// RunItem is an item the agent acted on during a run.
type RunItem struct {
	RunID  int
	ItemID int
	Title  string
	Status string
}

// NoteRunItem records that the running run acted on an item. It does nothing
// when no run is running (e.g. the MCP server used by hand).
func (s *Store) NoteRunItem(ctx context.Context, itemID int) error {
	_, err := s.Pool.Exec(ctx, `INSERT INTO run_items (run_id, item_id)
		SELECT id, $1 FROM runs WHERE status = 'running' ON CONFLICT DO NOTHING`, itemID)
	return err
}

// RunItems returns the items each of the given runs acted on, in the order it
// first touched them.
func (s *Store) RunItems(ctx context.Context, runIDs []int) (map[int][]RunItem, error) {
	rows, err := s.Pool.Query(ctx, `SELECT ri.run_id, i.id, i.title, i.status FROM run_items ri
		JOIN items i ON i.id = ri.item_id WHERE ri.run_id = ANY($1) ORDER BY ri.run_id, ri.created_at, i.id`, runIDs)
	if err != nil {
		return nil, err
	}
	items, err := pgx.CollectRows(rows, pgx.RowToStructByPos[RunItem])
	if err != nil {
		return nil, err
	}
	out := map[int][]RunItem{}
	for _, it := range items {
		out[it.RunID] = append(out[it.RunID], it)
	}
	return out, nil
}
