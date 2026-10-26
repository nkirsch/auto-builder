package gate

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/nkirsch/auto-builder/internal/db"
	"github.com/nkirsch/auto-builder/internal/github"
)

// syncOverlap re-reads a little before the last sync so nothing updated
// during it is missed; imports are idempotent.
const syncOverlap = 2 * time.Minute

// IssueSync brings members' GitHub issues and their comments onto the board,
// mirrors board items and replies to issues (except items tagged private),
// and closes an item's issue once it ships or closes. Issues and comments by
// non-members (the repo is public, and that includes the App's own) are
// ignored.
type IssueSync struct {
	Store   *db.Store
	GitHub  *github.Client
	Repo    string
	BaseURL string
	// Notified creates in-app notifications for an imported comment.
	Notified func(ctx context.Context, it db.Item, actor, body string)

	mu sync.Mutex // serialises Sync so overlapping syncs don't post a reply twice
}

func (s *IssueSync) Sync(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	last, err := s.Store.IssuesSyncedAt(ctx)
	if err != nil {
		return err
	}
	var since time.Time
	if last != nil {
		since = last.Add(-syncOverlap)
	}
	started := time.Now()
	issues, err := s.GitHub.OpenIssues(ctx, s.Repo, since)
	if err != nil {
		return fmt.Errorf("list issues: %w", err)
	}
	var errs []error
	for _, is := range issues {
		if err := s.importIssue(ctx, is); err != nil {
			errs = append(errs, fmt.Errorf("issue %d: %w", is.Number, err))
		}
	}
	comments, err := s.GitHub.IssueComments(ctx, s.Repo, since)
	if err != nil {
		return errors.Join(append(errs, fmt.Errorf("list comments: %w", err))...)
	}
	for _, c := range comments {
		if err := s.importComment(ctx, c); err != nil {
			errs = append(errs, fmt.Errorf("comment %d: %w", c.ID, err))
		}
	}
	if len(errs) == 0 {
		errs = append(errs, s.Store.SetIssuesSyncedAt(ctx, started))
	}
	return errors.Join(append(errs, s.mirrorItems(ctx), s.reopenIssues(ctx), s.mirrorReplies(ctx), s.closeIssues(ctx))...)
}

// maxIssueBody keeps mirrored text under GitHub's 65,536-character limit.
const maxIssueBody = 60_000

// itemRefRE matches a board reference #N not preceded by a word character,
// &, /, #, [ or "PR "; on GitHub #N would name an issue instead.
var itemRefRE = regexp.MustCompile(`(^|[^\w&/#\[])#([0-9]{1,9})\b`)

var prRefRE = regexp.MustCompile(`(?i)\b(?:PR|pull request) $`)

// forGitHub truncates board markdown and points its #N references at the board.
func (s *IssueSync) forGitHub(body string) string {
	if r := []rune(body); len(r) > maxIssueBody {
		body = string(r[:maxIssueBody]) + "…"
	}
	var b strings.Builder
	at := 0
	for _, m := range itemRefRE.FindAllStringSubmatchIndex(body, -1) {
		ref := m[3] // the #
		if prRefRE.MatchString(body[:ref]) {
			continue
		}
		n := body[m[4]:m[5]]
		fmt.Fprintf(&b, "%s[#%s](%s/items/%s)", body[at:ref], n, s.BaseURL, n)
		at = m[5]
	}
	return b.String() + body[at:]
}

// mirrorItems opens an issue for each active item that has none.
func (s *IssueSync) mirrorItems(ctx context.Context) error {
	items, err := s.Store.ItemsToMirror(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, it := range items {
		body := strings.TrimSpace(s.forGitHub(it.Body) +
			fmt.Sprintf("\n\n_Board item [#%d](%s/items/%d) by **%s**._", it.ID, s.BaseURL, it.ID, it.Author))
		n, err := s.GitHub.CreateIssue(ctx, s.Repo, it.Title, body)
		if err != nil {
			errs = append(errs, fmt.Errorf("mirror item %d: %w", it.ID, err))
			continue
		}
		slog.Info("mirrored item", "item", it.ID, "issue", n)
		errs = append(errs, s.Store.SetItemIssue(ctx, it.ID, n))
	}
	return errors.Join(errs...)
}

// mirrorReplies posts board replies that aren't on GitHub yet as issue comments.
func (s *IssueSync) mirrorReplies(ctx context.Context) error {
	replies, err := s.Store.RepliesToMirror(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, r := range replies {
		body := fmt.Sprintf("**%s** on board item [#%d](%s/items/%d):\n\n%s",
			r.Author, r.ItemID, s.BaseURL, r.ItemID, s.forGitHub(strings.TrimSpace(r.Body)))
		id, err := s.GitHub.Comment(ctx, s.Repo, r.Issue, body)
		if err != nil {
			errs = append(errs, fmt.Errorf("mirror reply %d: %w", r.ID, err))
			continue
		}
		errs = append(errs, s.Store.SetReplyIssueComment(ctx, r.ID, id))
	}
	return errors.Join(errs...)
}

// reopenIssues reopens the issue of an item reopened after its issue was closed.
func (s *IssueSync) reopenIssues(ctx context.Context) error {
	items, err := s.Store.IssuesToReopen(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, it := range items {
		if err := s.GitHub.ReopenIssue(ctx, s.Repo, *it.IssueNumber); err != nil {
			errs = append(errs, fmt.Errorf("reopen issue %d: %w", *it.IssueNumber, err))
			continue
		}
		errs = append(errs, s.Store.MarkIssueReopened(ctx, it.ID))
	}
	return errors.Join(errs...)
}

// member returns login's board spelling, or "" if they aren't a member.
func (s *IssueSync) member(ctx context.Context, login string) (string, error) {
	m, err := s.Store.Member(ctx, login)
	if errors.Is(err, db.ErrNotFound) {
		return "", nil
	}
	return m.Login, err
}

func (s *IssueSync) importIssue(ctx context.Context, is github.Issue) error {
	if is.PullRequest != nil {
		return nil
	}
	author, err := s.member(ctx, is.User.Login)
	if author == "" || err != nil {
		return err
	}
	title := strings.TrimSpace(is.Title)
	if r := []rune(title); len(r) > 200 {
		title = string(r[:200])
	}
	body := strings.TrimSpace(is.Body)
	if r := []rune(body); len(r) > 50_000 {
		body = string(r[:50_000])
	}
	body = strings.TrimSpace(body + fmt.Sprintf("\n\n_From GitHub issue [#%d](https://github.com/%s/issues/%d)._", is.Number, s.Repo, is.Number))
	id, created, err := s.Store.ImportIssue(ctx, is.Number, title, body, author)
	if created {
		slog.Info("imported issue", "issue", is.Number, "item", id, "author", author)
	}
	return err
}

func (s *IssueSync) importComment(ctx context.Context, c github.IssueComment) error {
	it, err := s.Store.ItemByIssue(ctx, c.IssueNumber())
	if errors.Is(err, db.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	author, err := s.member(ctx, c.User.Login)
	if author == "" || err != nil || strings.TrimSpace(c.Body) == "" {
		return err
	}
	created, err := s.Store.ImportIssueComment(ctx, it.ID, c.ID, author, strings.TrimSpace(c.Body))
	if created && s.Notified != nil {
		s.Notified(ctx, it, author, c.Body)
	}
	return err
}

func (s *IssueSync) closeIssues(ctx context.Context) error {
	items, err := s.Store.IssuesToClose(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, it := range items {
		note := fmt.Sprintf("Shipped; see board item #%d: %s/items/%d", it.ID, s.BaseURL, it.ID)
		if it.Status == "closed" {
			note = fmt.Sprintf("Closed on the board (item #%d): %s/items/%d", it.ID, s.BaseURL, it.ID)
		}
		if err := s.GitHub.CloseIssue(ctx, s.Repo, *it.IssueNumber, note, it.Status == "shipped"); err != nil {
			errs = append(errs, fmt.Errorf("close issue %d: %w", *it.IssueNumber, err))
			continue
		}
		errs = append(errs, s.Store.MarkIssueClosed(ctx, it.ID))
	}
	return errors.Join(errs...)
}
