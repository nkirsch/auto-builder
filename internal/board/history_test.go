package board

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nkirsch/auto-builder/internal/db"
	"github.com/nkirsch/auto-builder/internal/github"
)

func TestReleasesGroupsPRsUnderVersion(t *testing.T) {
	commits := []github.Commit{{SHA: "e"}, {SHA: "d"}, {SHA: "c"}, {SHA: "b"}, {SHA: "a"}}
	tags := map[string][]string{"d": {"v26.10.27-2"}, "b": {"v26.10.27-1"}}
	prs := []github.MergedPR{{Number: 5, SHA: "e"}, {Number: 4, SHA: "d"}, {Number: 3, SHA: "c"}, {Number: 1, SHA: "a"}, {Number: 9, SHA: "elsewhere"}}
	got := releases(commits, tags, prs)
	type row struct {
		tag string
		prs []int
	}
	var rows []row
	for _, r := range got {
		var n []int
		for _, p := range r.PRs {
			n = append(n, p.Number)
		}
		rows = append(rows, row{strings.Join(r.Tags, ","), n})
	}
	want := []row{{"", []int{5}}, {"v26.10.27-2", []int{4, 3}}, {"v26.10.27-1", []int{1}}}
	if !slices.EqualFunc(rows, want, func(a, b row) bool { return a.tag == b.tag && slices.Equal(a.prs, b.prs) }) {
		t.Errorf("releases = %+v, want %+v", rows, want)
	}
	if got := releases(commits[1:], tags, nil); len(got) != 2 || got[0].Tags[0] != "v26.10.27-2" {
		t.Errorf("versions without PRs = %+v, want both shown", got)
	}
}

func TestHistoryPage(t *testing.T) {
	b := &Board{}
	if err := b.parse(); err != nil {
		t.Fatal(err)
	}
	rs := []release{
		{PRs: []historyPR{{MergedPR: github.MergedPR{Number: 7, Title: "Next <thing>", Author: "bot", MergedAt: time.Now()}}}},
		{Tags: []string{"v26.10.27-1"}, SHA: "abcdef123", Live: true, PRs: []historyPR{
			{MergedPR: github.MergedPR{Number: 6, Title: "Dark mode", URL: "https://github.com/o/r/pull/6"}, Item: &db.PRItem{ID: 3, Author: "alice"}}}},
	}
	var sb strings.Builder
	p := page{Title: "History", Me: db.Member{Login: "x"}, Data: map[string]any{"Releases": rs, "Repo": "o/r", "Depth": 100}}
	if err := b.tmpl["history"].ExecuteTemplate(&sb, "layout", p); err != nil {
		t.Fatal(err)
	}
	out := sb.String()
	for _, want := range []string{
		`<a href="/history">History</a>`,
		`<b>Not released yet</b>`,
		`Next &lt;thing&gt;`,
		`<a class="tag" href="https://github.com/o/r/tree/v26.10.27-1">v26.10.27-1</a>`,
		`<span class="badge live">live</span>`,
		`<a href="https://github.com/o/r/pull/6">#6</a> Dark mode`,
		`<a href="/items/3">item #3</a> asked by <a href="/?by=alice" title="Only their asks">alice</a>`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("history page missing %s", want)
		}
	}
	if n := strings.Count(out, "badge live"); n != 1 {
		t.Errorf("live badges = %d, want 1", n)
	}
}

func TestMarkLive(t *testing.T) {
	rs := []release{{}, {Tags: []string{"v2"}, SHA: "b"}, {Tags: []string{"v1"}, SHA: "a"}}
	live := func(rs []release) (out []bool) {
		for _, r := range rs {
			out = append(out, r.Live)
		}
		return out
	}
	for _, c := range []struct {
		version, sha string
		want         []bool
	}{
		{"v2", "", []bool{false, true, false}},
		{"", "a", []bool{false, false, true}}, // a tenant's app, by its /health sha
		{"", "", []bool{false, false, false}},
	} {
		if got := live(markLive(rs, c.version, c.sha)); !slices.Equal(got, c.want) {
			t.Errorf("markLive(%q, %q) = %v, want %v", c.version, c.sha, got, c.want)
		}
	}
}
