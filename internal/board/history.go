package board

import (
	"context"
	"log/slog"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/nkirsch/auto-builder/internal/auth"
	"github.com/nkirsch/auto-builder/internal/db"
	"github.com/nkirsch/auto-builder/internal/github"
)

// historyDepth is how many default-branch commits and closed PRs the history page reads.
const historyDepth = 100

// historyTTL is how long the history page reuses what it read from GitHub.
const historyTTL = time.Minute

// release is one version on the history page and the PRs it shipped first.
type release struct {
	Tags []string // "" release: merged but not yet tagged
	SHA  string
	At   time.Time
	Live bool // the version this server runs
	PRs  []historyPR
}

type historyPR struct {
	github.MergedPR
	Item *db.PRItem
}

type historyCache struct {
	mu   sync.Mutex
	at   time.Time
	data []release
}

// releases groups prs under the newest-first commits: each PR goes to the
// nearest tagged commit at or above its merge commit. PRs whose merge commit
// isn't among commits are dropped.
func releases(commits []github.Commit, tags map[string][]string, prs []github.MergedPR) []release {
	bySHA := map[string][]github.MergedPR{}
	for _, p := range prs {
		bySHA[p.SHA] = append(bySHA[p.SHA], p)
	}
	var out []release
	cur := &release{}
	for _, c := range commits {
		if t := tags[c.SHA]; len(t) > 0 {
			if len(cur.PRs) > 0 || cur.SHA != "" {
				out = append(out, *cur)
			}
			slices.Sort(t)
			cur = &release{Tags: t, SHA: c.SHA, At: c.At}
		}
		for _, p := range bySHA[c.SHA] {
			cur.PRs = append(cur.PRs, historyPR{MergedPR: p})
		}
	}
	if len(cur.PRs) > 0 || cur.SHA != "" {
		out = append(out, *cur)
	}
	return out
}

func (b *Board) releases(ctx context.Context) ([]release, error) {
	b.history.mu.Lock()
	defer b.history.mu.Unlock()
	if b.history.data != nil && time.Since(b.history.at) < historyTTL {
		return b.history.data, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	commits, err := b.GitHub.Commits(ctx, b.Repo, historyDepth)
	if err != nil {
		return nil, err
	}
	tags, err := b.GitHub.CommitTags(ctx, b.Repo, "v")
	if err != nil {
		return nil, err
	}
	prs, err := b.GitHub.MergedPRs(ctx, b.Repo, historyDepth)
	if err != nil {
		return nil, err
	}
	rs := releases(commits, tags, prs)
	nums := make([]int, len(prs))
	for i, p := range prs {
		nums[i] = p.Number
	}
	items, err := b.Store.ItemsForPRs(ctx, nums)
	if err != nil {
		return nil, err
	}
	for _, r := range rs {
		for i, p := range r.PRs {
			if it, ok := items[p.Number]; ok {
				r.PRs[i].Item = &it
			}
		}
	}
	b.history.data, b.history.at = rs, time.Now()
	return rs, nil
}

// historyPage shows the PRs merged into the default branch, grouped by the version that first shipped them.
func (b *Board) historyPage(w http.ResponseWriter, r *http.Request) {
	data := map[string]any{"Depth": historyDepth, "Repo": b.Repo}
	if auth.Member(r.Context()).IsAdmin() {
		data["Release"] = b.releaseInfo(r.Context(), "/history")
	}
	if b.GitHub == nil {
		data["Err"] = "needs the GitHub App"
	} else if rs, err := b.releases(r.Context()); err != nil {
		slog.Warn("history", "err", err)
		data["Err"] = err.Error()
	} else {
		live, sha := b.Version(), b.GitSHA
		if b.Deployed != nil {
			live = ""
			if sha, err = b.Deployed(r.Context()); err != nil {
				slog.Warn("history deployed sha", "err", err)
			}
		}
		data["Releases"] = markLive(rs, live, sha)
	}
	b.render(w, r, "history", "History", data)
}

// markLive copies rs with Live set on the release tagged live or at sha.
func markLive(rs []release, live, sha string) []release {
	out := make([]release, len(rs))
	for i, rel := range rs {
		rel.Live = rel.SHA != "" && (live != "" && slices.Contains(rel.Tags, live) || sha != "" && rel.SHA == sha)
		out[i] = rel
	}
	return out
}
