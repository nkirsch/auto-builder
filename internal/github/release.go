package github

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata" // the distroless image has no zoneinfo
)

// pacific is the zone release tags are dated in.
var pacific, _ = time.LoadLocation("America/Los_Angeles")

// ReleaseHead cuts release vYY.M.D-N (dated now) at repo's default branch
// head, returning the tag and how many commits it adds; tag "" means nothing
// is unreleased. Made with an App token, it triggers release workflows.
func (c *Client) ReleaseHead(ctx context.Context, repo string, now time.Time) (tag string, ahead int, err error) {
	latest, err := c.LatestRelease(ctx, repo)
	if err != nil {
		return "", 0, err
	}
	head, ahead, err := c.Unreleased(ctx, repo, latest)
	if err != nil || ahead == 0 {
		return "", 0, err
	}
	day := ReleaseDay(now)
	existing, err := c.TagsWithPrefix(ctx, repo, day)
	if err != nil {
		return "", 0, err
	}
	tag = NextTag(day, existing)
	if _, err := c.CreateRelease(ctx, repo, tag, head); err != nil {
		return "", 0, err
	}
	return tag, ahead, nil
}

// ReleaseDay is the date part of a release tag, e.g. "v26.10.24" (Pacific, no zero padding).
func ReleaseDay(t time.Time) string {
	t = t.In(pacific)
	return fmt.Sprintf("v%d.%d.%d", t.Year()%100, int(t.Month()), t.Day())
}

// NextTag is day-N one past the highest N among existing; a bare day tag doesn't count.
func NextTag(day string, existing []string) string {
	n := 0
	for _, t := range existing {
		if s, ok := strings.CutPrefix(t, day+"-"); ok {
			if i, err := strconv.Atoi(s); err == nil && i > n {
				n = i
			}
		}
	}
	return fmt.Sprintf("%s-%d", day, n+1)
}
