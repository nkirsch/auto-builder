package gate

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/nkirsch/auto-builder/internal/github"
)

type memUpstream struct {
	on bool
	pr int
}

func (m *memUpstream) UpstreamSync(context.Context) (bool, int, error) { return m.on, m.pr, nil }
func (m *memUpstream) SetUpstreamPR(_ context.Context, pr int) error   { m.pr = pr; return nil }

// fakeRepos is GitHub for a fork o/fork of u/up: upstream's head, whether
// the fork has it, the merge branch, and one PR's state.
type fakeRepos struct {
	mu                     sync.Mutex
	contains, branch       bool
	prOpen, merged, passed bool
	calls                  []string
}

func (f *fakeRepos) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	call := r.Method + " " + r.URL.Path
	f.calls = append(f.calls, call)
	switch {
	case call == "GET /repos/u/up/commits/master":
		w.Write([]byte(`{"sha":"abcdef1234"}`))
	case call == "GET /repos/o/fork/compare/master...abcdef1234":
		if !f.contains {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(`{"status":"behind"}`))
	case call == "GET /repos/o/fork/pulls":
		if f.prOpen {
			w.Write([]byte(`[{"number":9}]`))
			return
		}
		w.Write([]byte(`[]`))
	case call == "GET /repos/o/fork/commits/upstream-abcdef1":
		if !f.branch {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(`{"sha":"merge1"}`))
	case call == "POST /repos/o/fork/pulls":
		var in map[string]string
		json.NewDecoder(r.Body).Decode(&in)
		if in["head"] != "upstream-abcdef1" || in["base"] != "master" {
			http.Error(w, "bad pr", http.StatusUnprocessableEntity)
			return
		}
		f.prOpen = true
		w.Write([]byte(`{"number":9}`))
	case call == "GET /repos/o/fork/pulls/9":
		state := "open"
		if f.merged {
			state = "closed"
		}
		json.NewEncoder(w).Encode(map[string]any{"number": 9, "state": state, "merged": f.merged, "head": map[string]string{"sha": "merge1"}})
	case call == "GET /repos/o/fork/commits/merge1/check-runs":
		conclusion := `"status":"in_progress","conclusion":null`
		if f.passed {
			conclusion = `"status":"completed","conclusion":"success"`
		}
		w.Write([]byte(`{"check_runs":[{` + conclusion + `}]}`))
	case call == "PUT /repos/o/fork/pulls/9/merge":
		var in map[string]string
		json.NewDecoder(r.Body).Decode(&in)
		if in["merge_method"] != "merge" {
			http.Error(w, "squash would drop upstream's history", http.StatusUnprocessableEntity)
			return
		}
		f.merged = true
		w.Write([]byte(`{}`))
	default:
		http.NotFound(w, r)
	}
}

func TestUpstream(t *testing.T) {
	ctx := context.Background()
	fake := &fakeRepos{}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	state := &memUpstream{}
	starts := 0
	u := &Upstream{State: state, GitHub: github.New(srv.URL, "t"), Repo: "o/fork", Upstream: "u/up", Branch: "master",
		Start: func(context.Context) error { starts++; return nil }}
	releases := 0
	u.Release = func(context.Context) error { releases++; return nil }

	step := func(want string) {
		t.Helper()
		fake.calls = nil
		if err := u.Check(ctx); err != nil {
			t.Fatalf("%s: %v", want, err)
		}
	}
	step("off")
	if len(fake.calls) != 0 {
		t.Fatalf("off but called %v", fake.calls)
	}
	state.on = true
	step("behind: start the merge")
	if starts != 1 {
		t.Fatalf("starts %d", starts)
	}
	step("worker still running: don't start again")
	if starts != 1 {
		t.Fatalf("restarted: %d", starts)
	}
	fake.branch = true
	step("branch pushed: open the PR")
	if !fake.prOpen || state.pr != 9 {
		t.Fatalf("pr open=%v state=%d", fake.prOpen, state.pr)
	}
	step("checks running: wait")
	if fake.merged || releases != 0 {
		t.Fatal("merged before checks passed")
	}
	fake.passed = true
	step("checks passed: merge")
	if !fake.merged || state.pr != 0 || releases != 1 {
		t.Fatalf("merged=%v pr=%d releases=%d", fake.merged, state.pr, releases)
	}
	fake.contains, fake.prOpen = true, false
	step("up to date")
	if starts != 1 || strings.Contains(strings.Join(fake.calls, " "), "POST") {
		t.Fatalf("acted while up to date: %v", fake.calls)
	}
}

// A PR merged by hand, not by the hub, still gets its release.
func TestUpstreamMergedByHand(t *testing.T) {
	fake := &fakeRepos{prOpen: true, merged: true}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	state := &memUpstream{on: true, pr: 9}
	releases := 0
	u := &Upstream{State: state, GitHub: github.New(srv.URL, "t"), Repo: "o/fork", Upstream: "u/up", Branch: "master",
		Start: func(context.Context) error { return nil }, Release: func(context.Context) error { releases++; return nil }}
	if err := u.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	if state.pr != 0 || releases != 1 {
		t.Fatalf("pr=%d releases=%d, want 0 and 1", state.pr, releases)
	}
}
