package gate

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/nkirsch/auto-builder/internal/db"
	"github.com/nkirsch/auto-builder/internal/github"
)

// Shipper moves building → merged → shipped from GitHub state and the
// server's own GIT_SHA. It is deterministic; no agent involved.
type Shipper struct {
	Store  *db.Store
	GitHub *github.Client
	Repo   string
	GitSHA string
	// Deployed, when set, reports the live SHA instead of GitSHA: a tenant's
	// app deploys apart from this server (TENANCY.md).
	Deployed func(ctx context.Context) (string, error)
	// Reply posts a bot reply on an item.
	Reply func(ctx context.Context, it db.Item, body string) error
}

func (s *Shipper) Check(ctx context.Context) error {
	items, err := s.Store.Items(ctx, "building", "merged")
	if err != nil {
		return err
	}
	var errs []error
	for _, it := range items {
		if err := s.checkItem(ctx, it); err != nil {
			errs = append(errs, fmt.Errorf("item %d: %w", it.ID, err))
		}
	}
	return errors.Join(errs...)
}

func (s *Shipper) checkItem(ctx context.Context, it db.Item) error {
	switch it.Status {
	case "building":
		if it.PRNumber == nil {
			return nil
		}
		pr, err := s.GitHub.PR(ctx, s.Repo, *it.PRNumber)
		if err != nil {
			return err
		}
		switch {
		case pr.Merged:
			if err := s.Store.MarkMerged(ctx, it.ID, pr.MergeCommitSHA); err != nil {
				return err
			}
			slog.Info("item merged", "item", it.ID, "pr", pr.Number, "sha", pr.MergeCommitSHA)
			it.Status, it.MergeSHA = "merged", pr.MergeCommitSHA
			return s.ship(ctx, it)
		case pr.State == "closed":
			if err := s.Store.PRClosed(ctx, it.ID); err != nil {
				return err
			}
			note := fmt.Sprintf("PR #%d was closed without merging.", pr.Number)
			if err := s.Store.SetBlocked(ctx, it.ID, note, false); err != nil {
				return err
			}
			return s.Reply(ctx, it, fmt.Sprintf("PR #%d was closed without merging; this is open again.", pr.Number))
		case it.Blocked == "" && pr.Head.SHA != "":
			failed, err := s.GitHub.ChecksFailed(ctx, s.Repo, pr.Head.SHA)
			if err != nil || !failed {
				return err
			}
			slog.Info("item blocked: CI failed", "item", it.ID, "pr", pr.Number)
			return s.Store.SetBlocked(ctx, it.ID, fmt.Sprintf("CI failed on PR #%d.", pr.Number), false)
		}
	case "merged":
		return s.ship(ctx, it)
	}
	return nil
}

func (s *Shipper) ship(ctx context.Context, it db.Item) error {
	live := s.GitSHA
	if s.Deployed != nil {
		var err error
		if live, err = s.Deployed(ctx); err != nil {
			return fmt.Errorf("deployed sha: %w", err)
		}
	}
	if it.MergeSHA == "" || live == "" || live == "dev" {
		return nil
	}
	ok, err := s.GitHub.Contains(ctx, s.Repo, live, it.MergeSHA)
	if err != nil || !ok {
		return err
	}
	if err := s.Store.MarkShipped(ctx, it.ID); err != nil {
		return err
	}
	slog.Info("item shipped", "item", it.ID, "sha", it.MergeSHA)
	return s.Reply(ctx, it, s.shippedNote(ctx, it.MergeSHA))
}

// shippedNote names the release, e.g. "Shipped in v26.10.25-3 (abc1234).",
// falling back to the short SHA when the version lookup fails.
func (s *Shipper) shippedNote(ctx context.Context, sha string) string {
	v, err := s.GitHub.Version(ctx, s.Repo, sha)
	if err != nil {
		slog.Warn("ship version", "sha", sha, "err", err)
		return "Shipped in " + short(sha) + "."
	}
	return fmt.Sprintf("Shipped in %s (%s).", v, short(sha))
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}
