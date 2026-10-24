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
			return s.Reply(ctx, it, fmt.Sprintf("PR #%d was closed without merging; this is open again.", pr.Number))
		}
	case "merged":
		return s.ship(ctx, it)
	}
	return nil
}

func (s *Shipper) ship(ctx context.Context, it db.Item) error {
	if it.MergeSHA == "" || s.GitSHA == "" || s.GitSHA == "dev" {
		return nil
	}
	ok, err := s.GitHub.Contains(ctx, s.Repo, s.GitSHA, it.MergeSHA)
	if err != nil || !ok {
		return err
	}
	if err := s.Store.MarkShipped(ctx, it.ID); err != nil {
		return err
	}
	slog.Info("item shipped", "item", it.ID, "sha", it.MergeSHA)
	return s.Reply(ctx, it, "Shipped in "+short(it.MergeSHA)+".")
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}
