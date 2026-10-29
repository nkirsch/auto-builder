package board

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
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
}

func (b *Board) releaseInfo(ctx context.Context) *releaseInfo {
	if !b.Releases || b.GitHub == nil {
		return nil
	}
	if b.Ready != nil && !b.Ready(ctx) {
		return &releaseInfo{NoRepo: true}
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	ri := &releaseInfo{}
	latest, err := b.GitHub.LatestRelease(ctx, b.Repo)
	if err == nil {
		ri.Latest = latest
		_, ri.Ahead, err = b.GitHub.Unreleased(ctx, b.Repo, latest)
	}
	if github.IsNotFound(err) { // commits/HEAD: no repo or no commits yet
		return &releaseInfo{NoRepo: true}
	}
	if err != nil {
		slog.Warn("release info", "err", err)
		ri.Err = err.Error()
	}
	return ri
}

// release cuts vYY.M.D[-N] at the default branch's head. Made with the App's
// token, the release triggers the repo's release workflows.
func (b *Board) release(w http.ResponseWriter, r *http.Request) {
	if !b.Releases || b.GitHub == nil {
		http.NotFound(w, r)
		return
	}
	ctx := r.Context()
	if b.Ready != nil && !b.Ready(ctx) {
		redirect(w, r, "/runs", "The repo isn't set up yet")
		return
	}
	tag, ahead, err := b.GitHub.ReleaseHead(ctx, b.Repo, time.Now())
	if github.IsNotFound(err) {
		redirect(w, r, "/runs", "The repo isn't set up yet")
		return
	}
	if err != nil {
		redirect(w, r, "/runs", "Release failed: "+err.Error())
		return
	}
	if tag == "" {
		redirect(w, r, "/runs", "Nothing to release")
		return
	}
	slog.Info("release", "tag", tag, "by", auth.Member(ctx).Login)
	redirect(w, r, "/runs", fmt.Sprintf("Released %s (%d commits); deploying", tag, ahead))
}
