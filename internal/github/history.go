package github

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Commit is one commit on the default branch, newest first in Commits.
type Commit struct {
	SHA string
	At  time.Time
}

// Commits lists the newest n (at most 100) commits on repo's default branch.
func (c *Client) Commits(ctx context.Context, repo string, n int) ([]Commit, error) {
	var list []struct {
		SHA    string `json:"sha"`
		Commit struct {
			Committer struct {
				Date time.Time `json:"date"`
			} `json:"committer"`
		} `json:"commit"`
	}
	q := url.Values{"per_page": {fmt.Sprint(min(n, 100))}}
	if err := c.do(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/commits?%s", repo, q.Encode()), nil, &list); err != nil {
		return nil, err
	}
	out := make([]Commit, len(list))
	for i, l := range list {
		out[i] = Commit{SHA: l.SHA, At: l.Commit.Committer.Date}
	}
	return out, nil
}

// CommitTags maps each commit SHA to the lightweight tags starting with prefix
// that point at it.
func (c *Client) CommitTags(ctx context.Context, repo, prefix string) (map[string][]string, error) {
	var refs []struct {
		Ref    string `json:"ref"`
		Object struct {
			SHA  string `json:"sha"`
			Type string `json:"type"`
		} `json:"object"`
	}
	if err := c.do(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/git/matching-refs/tags/%s", repo, prefix), nil, &refs); err != nil {
		return nil, err
	}
	tags := map[string][]string{}
	for _, r := range refs {
		if r.Object.Type == "commit" {
			tags[r.Object.SHA] = append(tags[r.Object.SHA], strings.TrimPrefix(r.Ref, "refs/tags/"))
		}
	}
	return tags, nil
}

// MergedPR is a pull request merged into repo.
type MergedPR struct {
	Number   int
	Title    string
	Author   string
	URL      string
	SHA      string // merge commit
	MergedAt time.Time
}

// MergedPRs lists PRs merged among the n (at most 100) most recently updated closed ones.
func (c *Client) MergedPRs(ctx context.Context, repo string, n int) ([]MergedPR, error) {
	var list []struct {
		Number   int        `json:"number"`
		Title    string     `json:"title"`
		HTMLURL  string     `json:"html_url"`
		MergedAt *time.Time `json:"merged_at"`
		SHA      string     `json:"merge_commit_sha"`
		User     struct {
			Login string `json:"login"`
		} `json:"user"`
	}
	q := url.Values{"state": {"closed"}, "sort": {"updated"}, "direction": {"desc"}, "per_page": {fmt.Sprint(min(n, 100))}}
	if err := c.do(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/pulls?%s", repo, q.Encode()), nil, &list); err != nil {
		return nil, err
	}
	var out []MergedPR
	for _, l := range list {
		if l.MergedAt != nil {
			out = append(out, MergedPR{Number: l.Number, Title: l.Title, Author: l.User.Login, URL: l.HTMLURL, SHA: l.SHA, MergedAt: *l.MergedAt})
		}
	}
	return out, nil
}
