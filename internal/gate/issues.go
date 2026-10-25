package gate

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/nkirsch/auto-builder/internal/db"
	"github.com/nkirsch/auto-builder/internal/github"
)

// syncOverlap re-reads a little before the last sync so nothing updated
// during it is missed; imports are idempotent.
const syncOverlap = 2 * time.Minute

// IssueSync brings members' GitHub issues and their comments onto the board,
// and closes an imported issue once its item ships or closes. Issues and
// comments by non-members (the repo is public) are ignored.
type IssueSync struct {
	Store   *db.Store
	GitHub  *github.Client
	Repo    string
	BaseURL string
	// Notified creates in-app notifications for an imported comment.
	Notified func(ctx context.Context, it db.Item, actor, body string)
}

func (s *IssueSync) Sync(ctx context.Context) error {
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
	return errors.Join(append(errs, s.closeIssues(ctx))...)
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
