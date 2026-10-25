package board

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata" // the distroless image has no zoneinfo

	"github.com/nkirsch/auto-builder/internal/auth"
)

// pacific is the zone release tags are dated in.
var pacific, _ = time.LoadLocation("America/Los_Angeles")

// releaseInfo is the admin page's release section.
type releaseInfo struct {
	Latest string // latest release tag, "" if none
	Ahead  int    // commits on the default branch since it
	Err    string
}

func (b *Board) releaseInfo(ctx context.Context) *releaseInfo {
	if !b.Releases || b.GitHub == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	ri := &releaseInfo{}
	latest, err := b.GitHub.LatestRelease(ctx, b.Repo)
	if err == nil {
		ri.Latest = latest
		_, ri.Ahead, err = b.GitHub.Unreleased(ctx, b.Repo, latest)
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
	latest, err := b.GitHub.LatestRelease(ctx, b.Repo)
	if err != nil {
		redirect(w, r, "/admin", "Release failed: "+err.Error())
		return
	}
	head, ahead, err := b.GitHub.Unreleased(ctx, b.Repo, latest)
	if err != nil {
		redirect(w, r, "/admin", "Release failed: "+err.Error())
		return
	}
	if ahead == 0 {
		redirect(w, r, "/admin", "Nothing to release since "+latest)
		return
	}
	day := releaseDay(time.Now())
	existing, err := b.GitHub.TagsWithPrefix(ctx, b.Repo, day)
	if err != nil {
		redirect(w, r, "/admin", "Release failed: "+err.Error())
		return
	}
	tag := nextTag(day, existing)
	if _, err := b.GitHub.CreateRelease(ctx, b.Repo, tag, head); err != nil {
		redirect(w, r, "/admin", "Release failed: "+err.Error())
		return
	}
	slog.Info("release", "tag", tag, "sha", head, "by", auth.Member(ctx).Login)
	redirect(w, r, "/admin", fmt.Sprintf("Released %s (%d commits); deploying", tag, ahead))
}

// releaseDay is the date part of a release tag, e.g. "v26.10.24" (Pacific, no zero padding).
func releaseDay(t time.Time) string {
	t = t.In(pacific)
	return fmt.Sprintf("v%d.%d.%d", t.Year()%100, int(t.Month()), t.Day())
}

// nextTag is day if unused, else day-N one past the highest N among existing.
func nextTag(day string, existing []string) string {
	taken, n := false, 0
	for _, t := range existing {
		if t == day {
			taken = true
		}
		if s, ok := strings.CutPrefix(t, day+"-"); ok {
			if i, err := strconv.Atoi(s); err == nil && i > n {
				n = i
			}
			taken = true
		}
	}
	if !taken {
		return day
	}
	return fmt.Sprintf("%s-%d", day, n+1)
}
