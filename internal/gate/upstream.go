package gate

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/nkirsch/auto-builder/internal/github"
	"github.com/nkirsch/auto-builder/internal/upstream"
)

// UpstreamState holds a fork's sync switch and the upstream PR in flight.
type UpstreamState interface {
	UpstreamSync(ctx context.Context) (on bool, pr int, err error)
	SetUpstreamPR(ctx context.Context, pr int) error
}

// restartAfter spaces out merge workers while a branch hasn't appeared.
const restartAfter = 30 * time.Minute

// Upstream keeps a fork current: when Upstream's Branch has commits Repo
// lacks, Start runs a worker that merges them into an "upstream-<sha>"
// branch (internal/upstream); Check then opens a PR from it and merges it
// with a merge commit once its checks pass. Deterministic; no agent.
type Upstream struct {
	State    UpstreamState
	GitHub   *github.Client
	Repo     string // the fork
	Upstream string // e.g. nkirsch/auto-builder
	Branch   string // both repos' default branch
	Start    func(ctx context.Context) error
	// Release, when set (a repo that deploys on release), cuts a release
	// once the upstream PR merges, so the merge deploys.
	Release func(ctx context.Context) error

	mu      sync.Mutex
	started time.Time
}

// Check advances the sync by one step; it's called on each tick.
func (u *Upstream) Check(ctx context.Context) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	on, pr, err := u.State.UpstreamSync(ctx)
	if err != nil || !on {
		return err
	}
	if pr != 0 {
		return u.land(ctx, pr)
	}
	head, err := u.GitHub.Head(ctx, u.Upstream, u.Branch)
	if err != nil {
		return fmt.Errorf("upstream head: %w", err)
	}
	if ok, err := u.GitHub.Contains(ctx, u.Repo, u.Branch, head); err != nil && !github.IsNotFound(err) {
		return err
	} else if ok {
		return nil
	}
	name := upstream.Branch(head)
	n, err := u.GitHub.OpenPR(ctx, u.Repo, name)
	if err != nil {
		return err
	}
	if n == 0 {
		if _, err := u.GitHub.Head(ctx, u.Repo, name); err == nil {
			title := fmt.Sprintf("Merge %s upstream (%s)", u.Upstream, head[:7])
			if n, err = u.GitHub.CreatePR(ctx, u.Repo, name, u.Branch, title,
				"Automatic upstream merge: fork-owned files keep this repo's copy. It merges once CI passes."); err != nil {
				return fmt.Errorf("open upstream PR: %w", err)
			}
			slog.Info("upstream PR opened", "repo", u.Repo, "pr", n)
		}
	}
	if n != 0 {
		return u.State.SetUpstreamPR(ctx, n)
	}
	if time.Since(u.started) < restartAfter {
		return nil
	}
	u.started = time.Now()
	slog.Info("upstream merge starting", "repo", u.Repo, "upstream", u.Upstream, "head", head)
	return u.Start(ctx)
}

// land merges PR n once its checks pass, and forgets it once it's merged or closed.
func (u *Upstream) land(ctx context.Context, n int) error {
	pr, err := u.GitHub.PR(ctx, u.Repo, n)
	if err != nil {
		return err
	}
	switch {
	case pr.State == "closed" && !pr.Merged:
		slog.Info("upstream PR closed", "repo", u.Repo, "pr", n)
		return u.State.SetUpstreamPR(ctx, 0)
	case pr.Merged:
		// Merged by hand: it still needs the release that deploys it.
		slog.Info("upstream PR merged elsewhere", "repo", u.Repo, "pr", n)
	default:
		ok, err := u.GitHub.ChecksPassed(ctx, u.Repo, pr.Head.SHA)
		if err != nil || !ok {
			return err
		}
		if err := u.GitHub.MergePR(ctx, u.Repo, n, fmt.Sprintf("Merge %s upstream (#%d)", u.Upstream, n)); err != nil {
			return fmt.Errorf("merge upstream PR %d: %w", n, err)
		}
		slog.Info("upstream PR merged", "repo", u.Repo, "pr", n)
	}
	if err := u.State.SetUpstreamPR(ctx, 0); err != nil {
		return err
	}
	if u.Release != nil {
		if err := u.Release(ctx); err != nil {
			return fmt.Errorf("release after upstream PR %d: %w", n, err)
		}
	}
	return nil
}
