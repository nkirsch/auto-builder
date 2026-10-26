package upstream

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// shell runs commands for real; remotes are rewritten to local repos.
type shell struct{ remotes map[string]string }

func (s shell) Run(ctx context.Context, env []string, args ...string) (string, error) {
	for i, a := range args {
		if local, ok := s.remotes[a]; ok {
			args[i] = local
		}
	}
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func run(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
}

func write(t *testing.T, dir, name, body string) {
	t.Helper()
	os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o755)
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A fork merges upstream's changes but keeps its own copy of a fork-owned
// file both sides changed; a second merge finds nothing to do.
func TestMerge(t *testing.T) {
	root := t.TempDir()
	up, fork := filepath.Join(root, "up"), filepath.Join(root, "fork.git")
	env := append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@x", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@x")
	t.Setenv("GIT_AUTHOR_NAME", "t")
	t.Setenv("GIT_AUTHOR_EMAIL", "t@x")
	t.Setenv("GIT_COMMITTER_NAME", "t")
	t.Setenv("GIT_COMMITTER_EMAIL", "t@x")

	run(t, root, "init", "-q", "-b", "master", up)
	write(t, up, ".gitattributes", "app.env merge=ours\nlayouts/** merge=ours\n")
	write(t, up, "app.env", "SITE=upstream\n")
	write(t, up, "code.go", "v1\n")
	run(t, up, "add", "-A")
	run(t, up, "commit", "-q", "-m", "v1")
	run(t, root, "clone", "-q", "--bare", up, fork)

	// The fork changes its own settings; upstream changes code and settings.
	work := filepath.Join(root, "work")
	run(t, root, "clone", "-q", fork, work)
	write(t, work, "app.env", "SITE=fork\n")
	run(t, work, "commit", "-q", "-am", "fork settings")
	run(t, work, "push", "-q", "origin", "master")
	write(t, up, "code.go", "v2\n")
	write(t, up, "app.env", "SITE=upstream2\n")
	write(t, up, "layouts/index.html", "redirect to iterate/\n") // new, fork-owned
	write(t, up, "new.go", "v2\n")                               // new, upstream's
	run(t, up, "add", "-A")
	run(t, up, "commit", "-q", "-m", "v2")

	sh := shell{remotes: map[string]string{"https://github.com/o/fork.git": fork, "https://github.com/o/up.git": up}}
	branch, err := Merge(context.Background(), sh, env, filepath.Join(root, "m1"), "o/fork", "o/up", "master")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(branch, "upstream-") {
		t.Fatalf("branch %q", branch)
	}
	check := filepath.Join(root, "check")
	run(t, root, "clone", "-q", "--branch", branch, fork, check)
	if b, _ := os.ReadFile(filepath.Join(check, "code.go")); string(b) != "v2\n" {
		t.Errorf("code.go %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(check, "app.env")); string(b) != "SITE=fork\n" {
		t.Errorf("app.env %q, want the fork's", b)
	}
	if _, err := os.Stat(filepath.Join(check, "layouts/index.html")); err == nil {
		t.Error("upstream's new fork-owned layouts/index.html landed in the fork")
	}
	if _, err := os.Stat(filepath.Join(check, "new.go")); err != nil {
		t.Errorf("upstream's new file missing: %v", err)
	}

	// Once the branch lands, the fork is up to date.
	run(t, check, "push", "-q", "origin", branch+":master")
	if branch, err := Merge(context.Background(), sh, env, filepath.Join(root, "m2"), "o/fork", "o/up", "master"); err != nil || branch != "" {
		t.Fatalf("second merge: %q %v", branch, err)
	}
}
