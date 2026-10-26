// Package upstream merges a fork's upstream into a branch, keeping the
// fork-owned files (.gitattributes merge=ours), as `make merge-upstream`
// does. The runner (a board's repo) and the provisioner (the platform repo)
// run it; the server opens and lands the PR.
package upstream

import (
	"context"
	"fmt"
	"strings"
)

// Shell runs a command and returns its stdout.
type Shell interface {
	Run(ctx context.Context, env []string, args ...string) (string, error)
}

// Branch names the merge branch for an upstream commit.
func Branch(sha string) string { return "upstream-" + sha[:min(7, len(sha))] }

// Merge clones repo into dir, merges upstream's branch and pushes the result
// to Branch(upstream head). env must let git push to repo. It returns ""
// when repo already contains upstream.
func Merge(ctx context.Context, sh Shell, env []string, dir, repo, upstream, branch string) (string, error) {
	git := func(args ...string) (string, error) {
		return sh.Run(ctx, env, append([]string{"git", "-C", dir}, args...)...)
	}
	if _, err := sh.Run(ctx, env, "git", "clone", "--quiet", "https://github.com/"+repo+".git", dir); err != nil {
		return "", err
	}
	for _, args := range [][]string{
		{"config", "merge.ours.driver", "true"},
		{"remote", "add", "upstream", "https://github.com/" + upstream + ".git"},
		{"fetch", "--quiet", "upstream", branch},
	} {
		if _, err := git(args...); err != nil {
			return "", err
		}
	}
	if _, err := git("merge-base", "--is-ancestor", "upstream/"+branch, "HEAD"); err == nil {
		return "", nil
	}
	head, err := git("rev-parse", "upstream/"+branch)
	if err != nil {
		return "", err
	}
	name := Branch(strings.TrimSpace(head))
	if _, err := git("switch", "--quiet", "-c", name); err != nil {
		return "", err
	}
	if _, err := git("merge", "--quiet", "--no-ff", "-X", "no-renames", "--no-edit", "upstream/"+branch); err != nil {
		files, _ := git("diff", "--name-only", "--diff-filter=U")
		return "", fmt.Errorf("conflicts merging %s: %s", upstream, strings.Join(strings.Fields(files), ", "))
	}
	// merge=ours only settles files both sides have; a file upstream adds under
	// a fork-owned path (say layouts/index.html, upstream's redirect page)
	// would land silently, so drop those.
	added, err := git("diff", "--name-only", "--diff-filter=A", "HEAD^1", "HEAD")
	if err != nil {
		return "", err
	}
	var owned []string
	for _, f := range strings.Fields(added) {
		if attr, err := git("check-attr", "merge", "--", f); err == nil && strings.HasSuffix(strings.TrimSpace(attr), ": ours") {
			owned = append(owned, f)
		}
	}
	if len(owned) > 0 {
		if _, err := git(append([]string{"rm", "--quiet", "--"}, owned...)...); err != nil {
			return "", err
		}
		if _, err := git("commit", "--quiet", "--amend", "--no-edit"); err != nil {
			return "", err
		}
	}
	if _, err := git("push", "--quiet", "--force", "origin", name); err != nil {
		return "", err
	}
	return name, nil
}
