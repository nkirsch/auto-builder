package board

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/nkirsch/auto-builder/internal/auth"
	"github.com/nkirsch/auto-builder/internal/github"
)

// releaseInfo is the runs page's release section, shown to admins.
type releaseInfo struct {
	Latest string // latest release tag, "" if none
	Ahead  int    // commits on the default branch since it
	// NoRepo: the repo has no commits yet (or doesn't exist, or the app is
	// still being provisioned), so there's nothing to release.
	NoRepo bool
	Err    string
	Back   string // the page the form returns to: "/runs" or "/history"
}

// releaseInfo reads the release section's state; back is the page its form
// returns to.
func (b *Board) releaseInfo(ctx context.Context, back string) *releaseInfo {
	if !b.Releases || b.GitHub == nil {
		return nil
	}
	if b.Ready != nil && !b.Ready(ctx) {
		return &releaseInfo{NoRepo: true, Back: back}
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	ri := &releaseInfo{Back: back}
	latest, err := b.GitHub.LatestRelease(ctx, b.Repo)
	if err == nil {
		ri.Latest = latest
		_, ri.Ahead, err = b.GitHub.Unreleased(ctx, b.Repo, latest)
	}
	if github.IsNotFound(err) { // commits/HEAD: no repo or no commits yet
		return &releaseInfo{NoRepo: true, Back: back}
	}
	if err != nil {
		slog.Warn("release info", "err", err)
		ri.Err = err.Error()
	}
	return ri
}

// pendingTTL is how long the nav reuses the count of unreleased commits.
const pendingTTL = time.Minute

type pendingCache struct {
	mu    sync.Mutex
	at    time.Time
	ahead int
}

// pendingRelease is the number of default-branch commits since the latest
// release, for the History badge: 0 when releases are off, the repo isn't
// ready, or GitHub can't say. Read at most once per pendingTTL.
func (b *Board) pendingRelease(ctx context.Context) int {
	if !b.Releases || b.GitHub == nil {
		return 0
	}
	b.pending.mu.Lock()
	defer b.pending.mu.Unlock()
	if !b.pending.at.IsZero() && time.Since(b.pending.at) < pendingTTL {
		return b.pending.ahead
	}
	ri := b.releaseInfo(ctx, "")
	b.pending.ahead = 0
	if ri != nil && ri.Err == "" && !ri.NoRepo {
		b.pending.ahead = ri.Ahead
	}
	b.pending.at = time.Now()
	return b.pending.ahead
}

// forgetReleases makes the next page read the unreleased count and the
// history afresh, after a release.
func (b *Board) forgetReleases() {
	b.pending.mu.Lock()
	b.pending.at = time.Time{}
	b.pending.mu.Unlock()
	b.history.mu.Lock()
	b.history.data = nil
	b.history.mu.Unlock()
}

// releaseBack is where the release form returns: the history page if it
// came from there, else the runs page.
func releaseBack(r *http.Request) string {
	if r.FormValue("back") == "/history" {
		return "/history"
	}
	return "/runs"
}

// release cuts vYY.M.D[-N] at the default branch's head. Made with the App's
// token, the release triggers the repo's release workflows.
func (b *Board) release(w http.ResponseWriter, r *http.Request) {
	if !b.Releases || b.GitHub == nil {
		http.NotFound(w, r)
		return
	}
	ctx := r.Context()
	back := releaseBack(r)
	if b.Ready != nil && !b.Ready(ctx) {
		redirect(w, r, back, "The repo isn't set up yet")
		return
	}
	tag, ahead, err := b.GitHub.ReleaseHead(ctx, b.Repo, time.Now())
	if github.IsNotFound(err) {
		redirect(w, r, back, "The repo isn't set up yet")
		return
	}
	if err != nil {
		redirect(w, r, back, "Release failed: "+err.Error())
		return
	}
	if tag == "" {
		redirect(w, r, back, "Nothing to release")
		return
	}
	b.forgetReleases()
	slog.Info("release", "tag", tag, "by", auth.Member(ctx).Login)
	redirect(w, r, back, fmt.Sprintf("Released %s (%d commits); deploying", tag, ahead))
}
