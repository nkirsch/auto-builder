package db

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNotFound is returned when a row doesn't exist.
var ErrNotFound = errors.New("not found")

// ErrRunActive is returned by CreateRun when a run is already in flight.
var ErrRunActive = errors.New("a run is already active")

// ErrSpendLimit is returned when the last 30 days' agent cost has reached the limit.
var ErrSpendLimit = errors.New("spend limit reached")

// ErrHeld is a launch the board allows but its platform holds (multi-tenant
// run cap or spend limit); the wrapping error says why.
var ErrHeld = errors.New("held")

// Store wraps the pool with the app's queries.
type Store struct{ Pool *pgxpool.Pool }

type Member struct {
	Login     string
	Role      string
	Name      string
	AvatarURL string
	Email     string // Google sign-in address; "" for GitHub-only members
	// GitHub: a member with an email whose login is also their GitHub
	// account, so they may sign in with either.
	GitHub   bool
	AddedBy  string
	AddedAt  time.Time
	LastSeen *time.Time
	// AutoApprove (admins only): their new asks, and items the agent files
	// from their comments, start approved.
	AutoApprove bool
}

func (m Member) IsAdmin() bool { return m.Role == "admin" }

// GitHubSignIn reports whether m may sign in with GitHub as their login: a
// member with an email only when the admin said the login is theirs there (it
// may be a made-up handle someone else owns).
func (m Member) GitHubSignIn() bool { return m.Email == "" || m.GitHub }

// Avatar returns AvatarURL sized for a 24px image: GitHub takes s=, Google =sN.
func (m Member) Avatar() string {
	u := m.AvatarURL
	switch {
	case strings.HasPrefix(u, "https://avatars.githubusercontent.com/"):
		if strings.Contains(u, "?") {
			return u + "&s=48"
		}
		return u + "?s=48"
	case strings.HasPrefix(u, "https://lh3.googleusercontent.com/"):
		if i := strings.LastIndex(u, "="); i > strings.LastIndex(u, "/") {
			u = u[:i]
		}
		return u + "=s48-c"
	}
	return u
}

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
	Tags            []string
	Blocked         string // why it needs action outside auto-builder; "" when not blocked
	BlockedAt       *time.Time
	IssueNumber     *int // GitHub issue it was imported from or mirrored to
	// Escalation is "admin" (a member asked the admins), "platform" (an admin
	// asked the platform admins) or "".
	Escalation  string
	EscalatedBy string
	EscalatedAt *time.Time
	Assignees   []string // logins, or addresses for members who sign in with Google
	Replies     int      // list queries only
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
	CostUSD     *float64 // nil when the runner didn't report one
	AgentAt     *time.Time
	ReportAt    *time.Time
	GitSHA      string // server release that claimed the run; "" before
	Version     string // its "vYY.MM.DD-N", "" if unknown at claim
}

// RunStageNames are a run's stages in order: queued (created → claimed),
// setup (token, clone), agent (Claude Code), report (log upload, finish).
var RunStageNames = []string{"queued", "setup", "agent", "report"}

// RunStage is one stage of a run. State is done, active, pending or skipped;
// Duration is so far for the active stage.
type RunStage struct {
	Name     string
	State    string
	Duration time.Duration
}

func (r Run) Stages() []RunStage { return r.stagesAt(time.Now()) }

func (r Run) stagesAt(now time.Time) []RunStage {
	created := r.CreatedAt
	starts := []*time.Time{&created, r.StartedAt, r.AgentAt, r.ReportAt}
	if r.FinishedAt != nil && r.AgentAt == nil && r.ReportAt == nil {
		return nil // finished before stages were recorded (or the runner never reported)
	}
	out := make([]RunStage, len(starts))
	for i, start := range starts {
		out[i].Name = RunStageNames[i]
		if start == nil {
			out[i].State = "pending"
			if r.FinishedAt != nil {
				out[i].State = "skipped"
			}
			continue
		}
		end := r.FinishedAt
		for _, next := range starts[i+1:] {
			if next != nil {
				end = next
				break
			}
		}
		if end == nil {
			out[i].State, out[i].Duration = "active", now.Sub(*start)
		} else {
			out[i].State, out[i].Duration = "done", end.Sub(*start)
		}
	}
	return out
}

// Progress is the percent of stages done; 100 once the run finished.
func (r Run) Progress() int {
	if r.FinishedAt != nil {
		return 100
	}
	done := 0
	for _, st := range r.Stages() {
		if st.State == "done" {
			done++
		}
	}
	return done * 100 / len(RunStageNames)
}

// Stage is the name of the active stage, or "" when none is.
func (r Run) Stage() string {
	for _, st := range r.Stages() {
		if st.State == "active" {
			return st.Name
		}
	}
	return ""
}

// Runtime is claim to finish, or to now while running; 0 before the claim.
func (r Run) Runtime() time.Duration { return r.runtimeAt(time.Now()) }

func (r Run) runtimeAt(now time.Time) time.Duration {
	if r.StartedAt == nil {
		return 0
	}
	if r.FinishedAt != nil {
		now = *r.FinishedAt
	}
	return now.Sub(*r.StartedAt)
}

func notFound(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

// Members

const memberCols = `login, role, name, avatar_url, email, github, added_by, added_at, last_seen, auto_approve`

func scanMember(row pgx.Row) (Member, error) {
	var m Member
	err := row.Scan(&m.Login, &m.Role, &m.Name, &m.AvatarURL, &m.Email, &m.GitHub, &m.AddedBy, &m.AddedAt, &m.LastSeen, &m.AutoApprove)
	return m, notFound(err)
}

// Member looks up a login case-insensitively (GitHub logins are).
func (s *Store) Member(ctx context.Context, login string) (Member, error) {
	return scanMember(s.Pool.QueryRow(ctx, `SELECT `+memberCols+` FROM members WHERE lower(login) = lower($1)`, login))
}

// MemberByEmail finds the member Google sign-in is for.
func (s *Store) MemberByEmail(ctx context.Context, email string) (Member, error) {
	if email == "" {
		return Member{}, ErrNotFound
	}
	return scanMember(s.Pool.QueryRow(ctx, `SELECT `+memberCols+` FROM members WHERE lower(email) = lower($1)`, email))
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

// ErrEmailTaken is returned when another member already has the email.
var ErrEmailTaken = errors.New("email belongs to another member")

// SetMemberEmail sets the address Google sign-in accepts for a member; "" removes it.
func (s *Store) SetMemberEmail(ctx context.Context, login, email string) error {
	err := s.execOne(ctx, `UPDATE members SET email = $2 WHERE lower(login) = lower($1)`, login, strings.TrimSpace(email))
	var pg *pgconn.PgError
	if errors.As(err, &pg) && pg.Code == "23505" {
		return ErrEmailTaken
	}
	return err
}

// SetMemberGitHub says whether a member with an email may also sign in with
// GitHub as their login.
func (s *Store) SetMemberGitHub(ctx context.Context, login string, on bool) error {
	return s.execOne(ctx, `UPDATE members SET github = $2 WHERE lower(login) = lower($1)`, login, on)
}

// SetAutoApprove turns auto-approve on or off for a member.
func (s *Store) SetAutoApprove(ctx context.Context, login string, on bool) error {
	return s.execOne(ctx, `UPDATE members SET auto_approve = $2 WHERE lower(login) = lower($1)`, login, on)
}

// AutoApprover returns the admin whose auto-approve covers an item split from
// item id: the latest author of the item or a reply on it who is an admin with
// auto-approve on, provided the item itself is approved; "" when none is.
func (s *Store) AutoApprover(ctx context.Context, id int) (string, error) {
	var login string
	err := s.Pool.QueryRow(ctx, `SELECT m.login FROM items i
		JOIN (SELECT item_id, author, created_at, id FROM responses
			UNION ALL SELECT id, author, created_at, 0 FROM items) p ON p.item_id = i.id
		JOIN members m ON lower(m.login) = lower(p.author) AND m.role = 'admin' AND m.auto_approve
		WHERE i.id = $1 AND i.approved
		ORDER BY p.created_at DESC, p.id DESC LIMIT 1`, id).Scan(&login)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return login, err
}

// ApproveBotItem approves an item the bot filed; not human activity.
func (s *Store) ApproveBotItem(ctx context.Context, id int, by string) error {
	return s.execOne(ctx, `UPDATE items SET approved = true, approved_by = $2 WHERE id = $1`, id, by)
}

func (s *Store) DeleteMember(ctx context.Context, login string) error {
	_, err := s.Pool.Exec(ctx, `DELETE FROM members WHERE lower(login) = lower($1)`, login)
	return err
}

// SetMailbox records login's connected Gmail (auth.Mailboxes).
func (s *Store) SetMailbox(ctx context.Context, login, address, token string) error {
	tag, err := s.Pool.Exec(ctx, `UPDATE members SET gmail_address = $2, gmail_token = $3
		WHERE lower(login) = lower($1)`, login, address, token)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return err
}

// Mailbox returns login's connected Gmail; "" when none.
func (s *Store) Mailbox(ctx context.Context, login string) (address, token string, err error) {
	err = s.Pool.QueryRow(ctx, `SELECT gmail_address, gmail_token FROM members WHERE lower(login) = lower($1)`,
		login).Scan(&address, &token)
	if errors.Is(err, pgx.ErrNoRows) {
		err = nil
	}
	return address, token, err
}

// TouchMember records a sign-in with the profile's name and avatar.
func (s *Store) TouchMember(ctx context.Context, login, name, avatar string) error {
	_, err := s.Pool.Exec(ctx, `UPDATE members SET name = $2, avatar_url = $3, last_seen = now()
		WHERE lower(login) = lower($1)`, login, name, avatar)
	return err
}

// Items

const itemCols = `i.id, i.title, i.body, i.author, i.status, i.approved, i.approved_by, i.pr_number,
	i.merge_sha, i.close_note, i.created_at, i.updated_at, i.human_activity_at, i.closed_at, i.tags,
	i.blocked, i.blocked_at, i.issue_number, i.escalation, i.escalated_by, i.escalated_at, i.assignees`

func scanItem(row pgx.Row, extra ...any) (Item, error) {
	var it Item
	dest := []any{&it.ID, &it.Title, &it.Body, &it.Author, &it.Status, &it.Approved, &it.ApprovedBy,
		&it.PRNumber, &it.MergeSHA, &it.CloseNote, &it.CreatedAt, &it.UpdatedAt, &it.HumanActivityAt, &it.ClosedAt, &it.Tags,
		&it.Blocked, &it.BlockedAt, &it.IssueNumber, &it.Escalation, &it.EscalatedBy, &it.EscalatedAt, &it.Assignees}
	err := row.Scan(append(dest, extra...)...)
	return it, notFound(err)
}

func (s *Store) Item(ctx context.Context, id int) (Item, error) {
	return scanItem(s.Pool.QueryRow(ctx, `SELECT `+itemCols+` FROM items i WHERE id = $1`, id))
}

// Items lists items in the given statuses (all when empty), newest activity first.
func (s *Store) Items(ctx context.Context, statuses ...string) ([]Item, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+itemCols+`, (SELECT count(*) FROM responses r WHERE r.item_id = i.id)
		FROM items i WHERE coalesce(cardinality($1::text[]), 0) = 0 OR i.status = ANY($1)
		ORDER BY i.updated_at DESC`, statuses)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Item, error) {
		var replies int
		it, err := scanItem(r, &replies)
		it.Replies = replies
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
	// epoch, not -infinity: pgx can't scan infinite timestamps into time.Time.
	err := s.Pool.QueryRow(ctx, `INSERT INTO items (title, body, author, human_activity_at)
		VALUES ($1, $2, $3, 'epoch') RETURNING id`, title, body, author).Scan(&id)
	return id, err
}

// CreateIssueItem files an item for GitHub issue number, numbered like the
// issue when that number is free on the board (otherwise the next free
// one). A bot's item doesn't count as human activity. created is false when
// the issue already has an item.
func (s *Store) CreateIssueItem(ctx context.Context, number int, title, body, author string, bot bool) (id int, created bool, err error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return 0, false, err
	}
	defer tx.Rollback(ctx)
	// epoch, not -infinity: pgx can't scan infinite timestamps into time.Time.
	activity := `CASE WHEN $5 THEN 'epoch'::timestamptz ELSE now() END`
	err = tx.QueryRow(ctx, `INSERT INTO items (id, title, body, author, issue_number, human_activity_at)
		SELECT $4, $1, $2, $3, $4, `+activity+` WHERE NOT EXISTS (SELECT 1 FROM items WHERE id = $4)
		ON CONFLICT DO NOTHING RETURNING id`, title, body, author, number, bot).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		err = tx.QueryRow(ctx, `INSERT INTO items (title, body, author, issue_number, human_activity_at)
			VALUES ($1, $2, $3, $4, `+activity+`) ON CONFLICT (issue_number) DO NOTHING RETURNING id`,
			title, body, author, number, bot).Scan(&id)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	// Numbered inserts skip the sequence; keep it past every item.
	if _, err := tx.Exec(ctx, `SELECT setval(pg_get_serial_sequence('items', 'id'), (SELECT max(id) FROM items))`); err != nil {
		return 0, false, err
	}
	return id, true, tx.Commit(ctx)
}

// ImportIssue files GitHub issue number as an item by its author (human
// activity). created is false when the issue was already imported.
func (s *Store) ImportIssue(ctx context.Context, number int, title, body, author string) (id int, created bool, err error) {
	return s.CreateIssueItem(ctx, number, title, body, author, false)
}

// ItemByIssue is the item imported from GitHub issue number.
func (s *Store) ItemByIssue(ctx context.Context, number int) (Item, error) {
	return scanItem(s.Pool.QueryRow(ctx, `SELECT `+itemCols+` FROM items i WHERE issue_number = $1`, number))
}

// IssuesToClose lists shipped or closed items whose issue the sync hasn't closed yet.
func (s *Store) IssuesToClose(ctx context.Context) ([]Item, error) {
	return s.itemList(ctx, `i.issue_number IS NOT NULL AND NOT i.issue_closed AND i.status IN ('shipped', 'closed')`)
}

func (s *Store) MarkIssueClosed(ctx context.Context, id int) error {
	return s.execOne(ctx, `UPDATE items SET issue_closed = true WHERE id = $1`, id)
}

// mirrored holds for items whose content may go to the public repo's issues.
const mirrored = `NOT ('private' = ANY(i.tags))`

// activeStatus holds for items that are neither shipped nor closed.
const activeStatus = `i.status IN ('open', 'building', 'merged')`

func (s *Store) itemList(ctx context.Context, where string) ([]Item, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+itemCols+` FROM items i WHERE `+where+` ORDER BY id`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Item, error) { return scanItem(r) })
}

// ItemsToMirror lists active items without a GitHub issue that aren't tagged
// private, filed at least a minute ago (tags are set just after the insert).
func (s *Store) ItemsToMirror(ctx context.Context) ([]Item, error) {
	return s.itemList(ctx, `i.issue_number IS NULL AND `+activeStatus+` AND `+mirrored+
		` AND i.created_at < now() - interval '1 minute'`)
}

// SetItemIssue records the GitHub issue item id was mirrored to (not activity).
func (s *Store) SetItemIssue(ctx context.Context, id, number int) error {
	return s.execOne(ctx, `UPDATE items SET issue_number = $2 WHERE id = $1 AND issue_number IS NULL`, id, number)
}

// IssuesToReopen lists active items whose issue the sync closed (an admin reopened them).
func (s *Store) IssuesToReopen(ctx context.Context) ([]Item, error) {
	return s.itemList(ctx, `i.issue_number IS NOT NULL AND i.issue_closed AND `+activeStatus+` AND `+mirrored)
}

func (s *Store) MarkIssueReopened(ctx context.Context, id int) error {
	return s.execOne(ctx, `UPDATE items SET issue_closed = false WHERE id = $1`, id)
}

// IssueReply is a board reply to mirror as a comment on Issue.
type IssueReply struct {
	Response
	Issue int
}

// RepliesToMirror lists non-empty replies, oldest first, on items with an
// issue that aren't tagged private, which aren't on GitHub yet (imported or
// already mirrored replies have issue_comment_id).
func (s *Store) RepliesToMirror(ctx context.Context) ([]IssueReply, error) {
	rows, err := s.Pool.Query(ctx, `SELECT r.id, r.item_id, r.author, r.body, r.created_at, i.issue_number
		FROM responses r JOIN items i ON i.id = r.item_id
		WHERE r.issue_comment_id IS NULL AND i.issue_number IS NOT NULL AND btrim(r.body) <> '' AND `+mirrored+`
		ORDER BY r.id`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (IssueReply, error) {
		var ir IssueReply
		err := r.Scan(&ir.ID, &ir.ItemID, &ir.Author, &ir.Body, &ir.CreatedAt, &ir.Issue)
		return ir, err
	})
}

// SetReplyIssueComment records the GitHub comment reply id was mirrored to.
func (s *Store) SetReplyIssueComment(ctx context.Context, id int, commentID int64) error {
	return s.execOne(ctx, `UPDATE responses SET issue_comment_id = $2 WHERE id = $1`, id, commentID)
}

// IssuesSyncedAt is when the last issue sync started; nil before the first.
func (s *Store) IssuesSyncedAt(ctx context.Context) (*time.Time, error) {
	var t *time.Time
	err := s.Pool.QueryRow(ctx, `SELECT issues_synced_at FROM settings`).Scan(&t)
	return t, err
}

func (s *Store) SetIssuesSyncedAt(ctx context.Context, t time.Time) error {
	return s.execOne(ctx, `UPDATE settings SET issues_synced_at = $1`, t)
}

// SetTags sets an item's tags. With onlyIfEmpty it leaves tagged items alone
// and returns ErrNotFound for them. It is not human activity either way.
func (s *Store) SetTags(ctx context.Context, id int, tags []string, onlyIfEmpty bool) error {
	if tags == nil {
		tags = []string{}
	}
	return s.execOne(ctx, `UPDATE items SET tags = $2 WHERE id = $1 AND (NOT $3 OR cardinality(tags) = 0)`,
		id, tags, onlyIfEmpty)
}

// SetBlocked flags an item as blocked with a note, or clears it when note is
// empty. Only a human (admin) clearing it counts as human activity.
func (s *Store) SetBlocked(ctx context.Context, id int, note string, human bool) error {
	return s.execOne(ctx, `UPDATE items SET blocked = $2,
		blocked_at = CASE WHEN $2 = '' THEN NULL ELSE now() END, updated_at = now(),
		human_activity_at = CASE WHEN $3 THEN now() ELSE human_activity_at END WHERE id = $1`, id, note, human)
}

// Escalate sets an item's escalation ("admin", "platform", or "" to clear
// it), who set it and whom it's assigned to; it's human activity.
func (s *Store) Escalate(ctx context.Context, id int, level, by string, assignees []string) error {
	if level == "" {
		by, assignees = "", nil
	}
	if assignees == nil {
		assignees = []string{}
	}
	return s.execOne(ctx, `UPDATE items SET escalation = $2, escalated_by = $3,
		escalated_at = CASE WHEN $2 = '' THEN NULL ELSE now() END, assignees = $4, updated_at = now(),
		human_activity_at = now() WHERE id = $1`, id, level, by, assignees)
}

// Escalated lists items escalated to level that aren't shipped or closed, oldest escalation first.
func (s *Store) Escalated(ctx context.Context, level string) ([]Item, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+itemCols+` FROM items i
		WHERE escalation = $1 AND status NOT IN ('shipped', 'closed') ORDER BY escalated_at`, level)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Item, error) { return scanItem(r) })
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
	return s.execOne(ctx, `UPDATE items SET status = 'merged', merge_sha = $2, blocked = '', blocked_at = NULL,
		updated_at = now() WHERE id = $1 AND status = 'building'`, id, sha)
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

// ImportIssueComment adds GitHub issue comment commentID as a human reply.
// created is false when the comment was already imported.
func (s *Store) ImportIssueComment(ctx context.Context, itemID int, commentID int64, author, body string) (created bool, err error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	tag, err := tx.Exec(ctx, `INSERT INTO responses (item_id, author, body, issue_comment_id) VALUES ($1, $2, $3, $4)
		ON CONFLICT (issue_comment_id) DO NOTHING`, itemID, author, body, commentID)
	if err != nil || tag.RowsAffected() == 0 {
		return false, err
	}
	if _, err := tx.Exec(ctx, `UPDATE items SET updated_at = now(), human_activity_at = now() WHERE id = $1`, itemID); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
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

// UnreadItems is the set of item IDs with unread notifications for the
// login, for the board's unread markers.
func (s *Store) UnreadItems(ctx context.Context, login string) (map[int]bool, error) {
	rows, err := s.Pool.Query(ctx, `SELECT DISTINCT item_id FROM notifications
		WHERE lower(login) = lower($1) AND read_at IS NULL`, login)
	if err != nil {
		return nil, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[int])
	if err != nil {
		return nil, err
	}
	set := make(map[int]bool, len(ids))
	for _, id := range ids {
		set[id] = true
	}
	return set, nil
}

// UnreadCount is the number of open board items (not shipped or closed) with
// unread notifications for the login.
func (s *Store) UnreadCount(ctx context.Context, login string) (int, error) {
	var n int
	err := s.Pool.QueryRow(ctx, `SELECT count(DISTINCT n.item_id) FROM notifications n JOIN items i ON i.id = n.item_id
		WHERE lower(n.login) = lower($1) AND n.read_at IS NULL AND i.status NOT IN ('shipped', 'closed')`,
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

const runCols = `id, status, trigger, requested_by, execution, summary, log_object, created_at, started_at, finished_at, cost_usd::float8,
	agent_at, report_at, git_sha, version`

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

// ClaimRun moves a requested run to running, recording the release (commit
// and version name) of the server that claimed it.
func (s *Store) ClaimRun(ctx context.Context, id int, sha, version string) error {
	return s.execOne(ctx, `UPDATE runs SET status = 'running', started_at = now(), git_sha = $2, version = $3
		WHERE id = $1 AND status = 'requested'`, id, sha, version)
}

// TakeRunToken marks a running run's one GitHub token as issued; ErrNotFound
// if the run isn't running or already had it, so the agent (which holds the
// runner token) can't mint more.
func (s *Store) TakeRunToken(ctx context.Context, id int) error {
	return s.execOne(ctx, `UPDATE runs SET token_at = now() WHERE id = $1 AND status = 'running' AND token_at IS NULL`, id)
}

// SetRunStage records when a running run reached stage agent or report; the
// first report wins.
func (s *Store) SetRunStage(ctx context.Context, id int, stage string) error {
	var col string
	switch stage {
	case "agent":
		col = "agent_at"
	case "report":
		col = "report_at"
	default:
		return fmt.Errorf("unknown stage %q", stage)
	}
	return s.execOne(ctx, `UPDATE runs SET `+col+` = coalesce(`+col+`, now()) WHERE id = $1 AND status = 'running'`, id)
}

// FinishRun records the outcome of an active run; cost may be nil. Items the
// summary lists (lines starting "#N") join the run's items, so ones the agent
// only checked or skipped show up too.
func (s *Store) FinishRun(ctx context.Context, id int, status, summary, logObject string, cost *float64) error {
	if err := s.execOne(ctx, `UPDATE runs SET status = $2, summary = $3, log_object = $4, cost_usd = $5,
		finished_at = now(), started_at = coalesce(started_at, now())
		WHERE id = $1 AND status IN ('requested', 'running')`,
		id, status, summary, logObject, cost); err != nil {
		return err
	}
	ids := SummaryItems(summary)
	if len(ids) == 0 {
		return nil
	}
	_, err := s.Pool.Exec(ctx, `INSERT INTO run_items (run_id, item_id)
		SELECT $1, id FROM items WHERE id = ANY($2) ON CONFLICT DO NOTHING`, id, ids)
	return err
}

// CancelRun stops an active run, returning it as it was (for its
// execution), or ErrNotFound when it isn't active.
func (s *Store) CancelRun(ctx context.Context, id int, by string) (Run, error) {
	r, err := s.Run(ctx, id)
	if err != nil {
		return r, err
	}
	return r, s.execOne(ctx, `UPDATE runs SET status = 'cancelled', summary = 'Cancelled by ' || $2 || '.',
		finished_at = now() WHERE id = $1 AND status IN ('requested', 'running')`, id, by)
}

// summaryItemRE matches an item reference at the start of a summary line,
// after any list marker or bold, e.g. "- **#12** (open): skipped".
var summaryItemRE = regexp.MustCompile(`(?m)^[\s*•-]*#(\d{1,9})\b`)

// SummaryItems returns the item ids a run summary lists, in order, once each.
func SummaryItems(summary string) []int {
	var ids []int
	seen := map[int]bool{}
	for _, m := range summaryItemRE.FindAllStringSubmatch(summary, -1) {
		if id, err := strconv.Atoi(m[1]); err == nil && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	return ids
}

// RunCosts is the reported spend across runs.
type RunCosts struct {
	Total, Last30Days float64
	Runs              int // runs with a reported cost
}

func (s *Store) RunCosts(ctx context.Context) (RunCosts, error) {
	var c RunCosts
	err := s.Pool.QueryRow(ctx, `SELECT
			coalesce(sum(cost_usd), 0)::float8,
			coalesce(sum(cost_usd) FILTER (WHERE created_at > now() - interval '30 days'), 0)::float8,
			count(cost_usd)
		FROM runs`).Scan(&c.Total, &c.Last30Days, &c.Runs)
	return c, err
}

// MemberCost is the agent spend attributed to one item author. A run's cost
// is split evenly across the items it acted on (run_items); runs that acted
// on no item stay unattributed.
type MemberCost struct {
	Login             string
	Total, Last30Days float64
}

func (s *Store) MemberCosts(ctx context.Context) ([]MemberCost, error) {
	rows, err := s.Pool.Query(ctx, `
		WITH share AS (
			SELECT ri.item_id, r.created_at, r.cost_usd / count(*) OVER (PARTITION BY r.id) AS usd
			FROM runs r JOIN run_items ri ON ri.run_id = r.id
			WHERE r.cost_usd IS NOT NULL)
		SELECT i.author, sum(sh.usd)::float8,
			coalesce(sum(sh.usd) FILTER (WHERE sh.created_at > now() - interval '30 days'), 0)::float8
		FROM share sh JOIN items i ON i.id = sh.item_id
		GROUP BY i.author ORDER BY 2 DESC, 1`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[MemberCost])
}

// SpendLimit is the admin-set cap on agent cost over the last 30 days.
func (s *Store) SpendLimit(ctx context.Context) (float64, error) {
	var v float64
	err := s.Pool.QueryRow(ctx, `SELECT spend_limit_usd::float8 FROM settings`).Scan(&v)
	return v, err
}

func (s *Store) SetSpendLimit(ctx context.Context, usd float64) error {
	return s.execOne(ctx, `UPDATE settings SET spend_limit_usd = $1`, usd)
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
	ApprovedOpen bool       // an approved open item exists, not blocked unless a human acted since
	Spend30      float64    // reported agent cost over the last 30 days
	SpendLimit   float64    // cap on Spend30
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
				AND human_activity_at > 'epoch' -- bot-filed items carry epoch
				AND human_activity_at > coalesce((SELECT t FROM last_ok), '-infinity')),
			EXISTS (SELECT 1 FROM items WHERE status = 'open' AND approved
				AND (blocked = '' OR human_activity_at > blocked_at)),
			(SELECT coalesce(sum(cost_usd), 0)::float8 FROM runs WHERE created_at > now() - interval '30 days'),
			(SELECT spend_limit_usd::float8 FROM settings)`).
		Scan(&g.Active, &g.LastSuccess, &g.LastFailure, &g.NewActivity, &g.ApprovedOpen, &g.Spend30, &g.SpendLimit)
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

// NoteRunPR records that the running run opened PR pr for an item. It does
// nothing when no run is running.
func (s *Store) NoteRunPR(ctx context.Context, itemID, pr int) error {
	_, err := s.Pool.Exec(ctx, `INSERT INTO run_prs (run_id, item_id, pr_number)
		SELECT id, $1, $2 FROM runs WHERE status = 'running' ON CONFLICT DO NOTHING`, itemID, pr)
	return err
}

// RunStats summarises finished runs created since a time. Durations are
// averages over the runs that recorded both ends; zero when none did.
type RunStats struct {
	Runs, Done, Failed int
	PRs, PRsMerged     int // PRs opened during the runs; merged = item merged or shipped with that PR
	Runtime            time.Duration
	MedianRuntime      time.Duration
	Stages             []RunStage // average per stage, in RunStageNames order
	CostPerPR          *float64   // reported cost / PRs; nil without PRs or cost
}

func (s *Store) RunStats(ctx context.Context, since time.Time) (RunStats, error) {
	var st RunStats
	var avg, median, queued, setup, agent, report float64
	var cost *float64
	err := s.Pool.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE status = 'done'), count(*) FILTER (WHERE status = 'failed'),
			coalesce(avg(extract(epoch FROM finished_at - started_at)), 0)::float8,
			coalesce(percentile_cont(0.5) WITHIN GROUP (ORDER BY extract(epoch FROM finished_at - started_at)), 0)::float8,
			coalesce(avg(extract(epoch FROM started_at - created_at)), 0)::float8,
			coalesce(avg(extract(epoch FROM agent_at - started_at)), 0)::float8,
			coalesce(avg(extract(epoch FROM report_at - agent_at)), 0)::float8,
			coalesce(avg(extract(epoch FROM finished_at - report_at)), 0)::float8,
			sum(cost_usd)::float8
		FROM runs WHERE status IN ('done', 'failed') AND created_at >= $1`, since).Scan(
		&st.Runs, &st.Done, &st.Failed, &avg, &median, &queued, &setup, &agent, &report, &cost)
	if err != nil {
		return st, err
	}
	err = s.Pool.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE i.pr_number = p.pr_number AND i.status IN ('merged', 'shipped'))
		FROM run_prs p JOIN runs r ON r.id = p.run_id JOIN items i ON i.id = p.item_id
		WHERE r.created_at >= $1`, since).Scan(&st.PRs, &st.PRsMerged)
	if err != nil {
		return st, err
	}
	secs := func(f float64) time.Duration { return time.Duration(f * float64(time.Second)).Round(time.Second) }
	st.Runtime, st.MedianRuntime = secs(avg), secs(median)
	for i, f := range []float64{queued, setup, agent, report} {
		st.Stages = append(st.Stages, RunStage{Name: RunStageNames[i], State: "done", Duration: secs(f)})
	}
	if cost != nil && st.PRs > 0 {
		c := *cost / float64(st.PRs)
		st.CostPerPR = &c
	}
	return st, nil
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

// UpstreamSync reports whether the board keeps up with its upstream and the
// upstream PR in flight (0 for none).
func (s *Store) UpstreamSync(ctx context.Context) (on bool, pr int, err error) {
	err = s.Pool.QueryRow(ctx, `SELECT upstream_sync, coalesce(upstream_pr, 0) FROM settings`).Scan(&on, &pr)
	return on, pr, err
}

// SetUpstreamSync turns keeping up with upstream on or off.
func (s *Store) SetUpstreamSync(ctx context.Context, on bool) error {
	_, err := s.Pool.Exec(ctx, `UPDATE settings SET upstream_sync = $1`, on)
	return err
}

// SetUpstreamPR records the upstream PR in flight; 0 clears it.
func (s *Store) SetUpstreamPR(ctx context.Context, pr int) error {
	_, err := s.Pool.Exec(ctx, `UPDATE settings SET upstream_pr = nullif($1, 0)`, pr)
	return err
}

// MembersIssue is the GitHub issue new members are welcomed on, 0 before the first.
func (s *Store) MembersIssue(ctx context.Context) (int, error) {
	var n int
	err := s.Pool.QueryRow(ctx, `SELECT coalesce(members_issue, 0) FROM settings`).Scan(&n)
	return n, err
}

func (s *Store) SetMembersIssue(ctx context.Context, n int) error {
	return s.execOne(ctx, `UPDATE settings SET members_issue = nullif($1, 0)`, n)
}
