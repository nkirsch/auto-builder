package board

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nkirsch/auto-builder/internal/db"
	"github.com/nkirsch/auto-builder/internal/github"
)

func TestIndexRunNowAdminOnly(t *testing.T) {
	b := &Board{}
	if err := b.parse(); err != nil {
		t.Fatal(err)
	}
	for role, want := range map[string]bool{"admin": true, "member": false} {
		var sb strings.Builder
		p := page{Title: "Board", Me: db.Member{Login: "x", Role: role}, Data: map[string]any{"Sections": nil, "Show": ""}}
		if err := b.tmpl["index"].ExecuteTemplate(&sb, "layout", p); err != nil {
			t.Fatal(err)
		}
		if got := strings.Contains(sb.String(), `<form method="post" action="/admin/run">`); got != want {
			t.Errorf("%s: Run now shown = %v, want %v", role, got, want)
		}
	}
}

func TestIndexUnread(t *testing.T) {
	b := &Board{}
	if err := b.parse(); err != nil {
		t.Fatal(err)
	}
	items := []db.Item{{ID: 3, Title: "x", Status: "open"}, {ID: 4, Title: "y", Status: "open"}}
	render := func(unread map[int]bool, count int) string {
		var sb strings.Builder
		p := page{Title: "Board", Me: db.Member{Login: "x"}, Unread: count,
			Data: map[string]any{"Sections": []section{{Items: items}}, "Show": "", "Unread": unread}}
		if err := b.tmpl["index"].ExecuteTemplate(&sb, "layout", p); err != nil {
			t.Fatal(err)
		}
		return sb.String()
	}
	out := render(map[int]bool{4: true}, 1)
	if n := strings.Count(out, `class="unread"`); n != 1 || !strings.Contains(out, `<li class="unread" title="Unread updates">
    <a href="/items/4"`) {
		t.Errorf("unread markers = %d, want item 4 only:\n%s", n, out)
	}
	if !strings.Contains(out, `action="/read"`) || !strings.Contains(out, `Board <span class="count"`) {
		t.Errorf("missing Mark all read or nav count")
	}
	if strings.Contains(out, "/inbox") {
		t.Errorf("page still links to the inbox")
	}
	out = render(nil, 0)
	if strings.Contains(out, `class="unread"`) || strings.Contains(out, `action="/read"`) || strings.Contains(out, `class="count"`) {
		t.Errorf("unread shown with nothing unread")
	}
}

func TestRunsListItems(t *testing.T) {
	b := &Board{}
	if err := b.parse(); err != nil {
		t.Fatal(err)
	}
	rows := []runRow{
		{Run: db.Run{ID: 2, Status: "running"}, Items: []db.RunItem{{RunID: 2, ItemID: 5, Title: "Chrome <toggle>", Status: "building"}}},
		{Run: db.Run{ID: 1, Status: "done"}},
	}
	var sb strings.Builder
	p := page{Title: "Runs", Me: db.Member{Login: "x"}, Data: map[string]any{"Runs": rows}}
	if err := b.tmpl["runs"].ExecuteTemplate(&sb, "layout", p); err != nil {
		t.Fatal(err)
	}
	out := sb.String()
	if want := `<a href="/items/5">#5 Chrome &lt;toggle&gt;</a> <span class="status s-building">building</span>`; !strings.Contains(out, want) {
		t.Errorf("runs page missing %s", want)
	}
	if n := strings.Count(out, `class="run-items"`); n != 1 {
		t.Errorf("run-items lists = %d, want 1", n)
	}
}

func TestIndexTags(t *testing.T) {
	b := &Board{}
	if err := b.parse(); err != nil {
		t.Fatal(err)
	}
	items := []db.Item{{ID: 8, Title: "Tags", Status: "open", Tags: []string{"feature"}}}
	var sb strings.Builder
	p := page{Title: "Board", Me: db.Member{Login: "x"}, Data: map[string]any{"Sections": sections(items, false), "Show": "", "Tag": "feature"}}
	if err := b.tmpl["index"].ExecuteTemplate(&sb, "layout", p); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`<a class="tag" href="/?tag=feature">#feature</a>`, `<a href="/">All tags</a>`,
		`<a class="button secondary" href="/?show=done&amp;tag=feature">Done</a>`} {
		if !strings.Contains(sb.String(), want) {
			t.Errorf("index missing %s", want)
		}
	}
}

func TestRunsAndAdminShowCost(t *testing.T) {
	b := &Board{}
	if err := b.parse(); err != nil {
		t.Fatal(err)
	}
	cost := 1.234
	var sb strings.Builder
	p := page{Title: "Runs", Me: db.Member{Login: "x"}, Data: map[string]any{"Runs": []runRow{{Run: db.Run{ID: 2, Status: "done", CostUSD: &cost}}, {Run: db.Run{ID: 1, Status: "done"}}}}}
	if err := b.tmpl["runs"].ExecuteTemplate(&sb, "layout", p); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(sb.String(), "$"); n != 1 || !strings.Contains(sb.String(), " · $1.23</span>") {
		t.Errorf("runs page: %d costs, want one $1.23", n)
	}

	sb.Reset()
	p = page{Title: "Admin", Me: db.Member{Login: "x", Role: "admin"}, Data: map[string]any{
		"Members":     []db.Member{{Login: "x", Role: "admin"}},
		"Costs":       db.RunCosts{Total: 12.5, Last30Days: 3, Runs: 4},
		"Limit":       20.0,
		"MemberCosts": []db.MemberCost{{Login: "alice", Total: 2.5, Last30Days: 1}},
	}}
	if err := b.tmpl["admin"].ExecuteTemplate(&sb, "layout", p); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Agent cost: $3.00 in the last 30 days, $12.50 in total (4 runs reported a cost).",
		`name="limit" type="number" min="0.01" step="0.01" value="20.00"`,
		"<b>alice</b> <span class=\"muted\">$1.00 in the last 30 days, $2.50 in total</span>",
	} {
		if !strings.Contains(sb.String(), want) {
			t.Errorf("admin page missing %q", want)
		}
	}
	if strings.Contains(sb.String(), "Spend limit reached") {
		t.Error("admin page says limit reached at $3 of $20")
	}

	sb.Reset()
	p.Data.(map[string]any)["Costs"] = db.RunCosts{Total: 30, Last30Days: 20, Runs: 9}
	if err := b.tmpl["admin"].ExecuteTemplate(&sb, "layout", p); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sb.String(), "Spend limit reached") {
		t.Error("admin page doesn't say limit reached at $20 of $20")
	}
}

func TestRunsShowStages(t *testing.T) {
	b := &Board{}
	if err := b.parse(); err != nil {
		t.Fatal(err)
	}
	t0 := time.Now().Add(-time.Hour)
	at := func(s int) *time.Time { v := t0.Add(time.Duration(s) * time.Second); return &v }
	rows := []runRow{
		{Run: db.Run{ID: 3, Status: "running", CreatedAt: t0, StartedAt: at(5), AgentAt: at(65)}},
		{Run: db.Run{ID: 2, Status: "done", CreatedAt: t0, StartedAt: at(5), AgentAt: at(65), ReportAt: at(785), FinishedAt: at(790)}},
		{Run: db.Run{ID: 1, Status: "requested", CreatedAt: t0}},
	}
	var sb strings.Builder
	p := page{Title: "Runs", Me: db.Member{Login: "x"}, Data: map[string]any{"Runs": rows}}
	if err := b.tmpl["runs"].ExecuteTemplate(&sb, "layout", p); err != nil {
		t.Fatal(err)
	}
	out := sb.String()
	for _, want := range []string{
		`<span class="ring r-running" style="--p: 50" title="50% of stages done, in agent">`,
		`<span class="ring r-done" style="--p: 100" title="100% of stages done">`,
		`<span class="ring r-requested" style="--p: 0"`,
		" · ran 13m 05s",
		`<span class="st-done">queued 5s</span><span class="st-done">setup 1m 00s</span><span class="st-done">agent 12m 00s</span><span class="st-done">report 5s</span>`,
		`<span class="st-active">agent `,
		`<span class="st-pending">report</span>`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("runs page missing %s", want)
		}
	}
	if strings.Count(out, " · running ") != 1 {
		t.Errorf("want one running runtime")
	}
}

func TestDur(t *testing.T) {
	for d, want := range map[time.Duration]string{
		45 * time.Second: "45s", 185 * time.Second: "3m 05s", 62*time.Minute + 10*time.Second: "1h 02m",
	} {
		if got := dur(d); got != want {
			t.Errorf("dur(%v) = %q, want %q", d, got, want)
		}
	}
}

func TestRunsShowRelease(t *testing.T) {
	b := &Board{}
	if err := b.parse(); err != nil {
		t.Fatal(err)
	}
	rows := []runRow{
		{Run: db.Run{ID: 3, Status: "done", GitSHA: "abc1234def", Version: "v26.10.25-3"}},
		{Run: db.Run{ID: 2, Status: "done", GitSHA: "fed9876cba"}},
		{Run: db.Run{ID: 1, Status: "done"}},
	}
	var sb strings.Builder
	p := page{Title: "Runs", Me: db.Member{Login: "x"}, Repo: "nkirsch/auto-builder", Data: map[string]any{"Runs": rows}}
	if err := b.tmpl["runs"].ExecuteTemplate(&sb, "layout", p); err != nil {
		t.Fatal(err)
	}
	out := sb.String()
	for _, want := range []string{
		`<b>Run 3</b>
      <a class="version" href="https://github.com/nkirsch/auto-builder/commit/abc1234def" title="abc1234def">v26.10.25-3</a>`,
		`<a class="version" href="https://github.com/nkirsch/auto-builder/commit/fed9876cba" title="fed9876cba">fed9876</a>`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("runs page missing %s", want)
		}
	}
	if n := strings.Count(out, `class="version"`); n != 2 {
		t.Errorf("versions shown = %d, want 2", n)
	}
}

func TestIndexSections(t *testing.T) {
	b := &Board{}
	if err := b.parse(); err != nil {
		t.Fatal(err)
	}
	items := []db.Item{
		{ID: 1, Title: "PR open", Status: "building", Approved: true},
		{ID: 2, Title: "Unapproved", Status: "open"},
		{ID: 3, Title: "Queued", Status: "open", Approved: true},
		{ID: 4, Title: "Merged", Status: "merged", Approved: true},
		{ID: 5, Title: "Also unapproved", Status: "open"},
		{ID: 6, Title: "Shipped, needs bootstrap", Status: "shipped", Approved: true, Blocked: "Re-run bootstrap."},
		{ID: 7, Title: "Building, CI failed", Status: "building", Approved: true, Blocked: "CI failed on PR #9."},
	}
	got := sections(items, false)
	var titles []string
	var ids [][]int
	for _, s := range got {
		titles = append(titles, s.Title)
		var row []int
		for _, it := range s.Items {
			row = append(row, it.ID)
		}
		ids = append(ids, row)
	}
	if !slices.Equal(titles, []string{"Blocked", "Waiting for approval", "Approved, not started", "In progress"}) {
		t.Fatalf("titles = %v", titles)
	}
	if fmt.Sprint(ids) != "[[6 7] [2 5] [3] [1 4]]" {
		t.Fatalf("ids = %v", ids)
	}
	if got := sections(items[1:2], false); len(got) != 1 || got[0].Title != "Waiting for approval" {
		t.Fatalf("empty sections kept: %+v", got)
	}
	if got := sections(items, true); len(got) != 1 || got[0].Title != "" || len(got[0].Items) != 7 {
		t.Fatalf("done view = %+v", got)
	}
	if len(sections(nil, true)) != 0 || len(sections(nil, false)) != 0 {
		t.Fatal("empty board has sections")
	}

	var sb strings.Builder
	p := page{Title: "Board", Me: db.Member{Login: "x"}, Data: map[string]any{"Sections": got, "Show": ""}}
	if err := b.tmpl["index"].ExecuteTemplate(&sb, "layout", p); err != nil {
		t.Fatal(err)
	}
	out := sb.String()
	a, q, w := strings.Index(out, "<h2>Waiting for approval</h2>"), strings.Index(out, "<h2>Approved, not started</h2>"), strings.Index(out, "<h2>In progress</h2>")
	if a < 0 || q < a || w < q || strings.Contains(out, "Nothing here.") {
		t.Fatalf("sections out of order or missing:\n%s", out)
	}
	if !strings.Contains(out[q:w], `href="/items/3"`) || !strings.Contains(out[w:], `href="/items/4"`) {
		t.Fatalf("items in wrong section")
	}
}

func TestIsDone(t *testing.T) {
	for _, tc := range []struct {
		it   db.Item
		want bool
	}{
		{db.Item{Status: "open"}, false},
		{db.Item{Status: "building"}, false},
		{db.Item{Status: "merged"}, false},
		{db.Item{Status: "shipped"}, true},
		{db.Item{Status: "closed"}, true},
		{db.Item{Status: "shipped", Blocked: "Re-run bootstrap."}, false},
		{db.Item{Status: "closed", Blocked: "Re-run bootstrap."}, false},
	} {
		if got := isDone(tc.it); got != tc.want {
			t.Errorf("isDone(%s, blocked %q) = %v, want %v", tc.it.Status, tc.it.Blocked, got, tc.want)
		}
	}
}

func TestRunsReleaseSection(t *testing.T) {
	b := &Board{}
	if err := b.parse(); err != nil {
		t.Fatal(err)
	}
	render := func(rel *releaseInfo) string {
		var sb strings.Builder
		p := page{Title: "Runs", Me: db.Member{Login: "x", Role: "admin"}, Data: map[string]any{"Runs": []runRow{}, "Release": rel}}
		if err := b.tmpl["runs"].ExecuteTemplate(&sb, "layout", p); err != nil {
			t.Fatal(err)
		}
		return sb.String()
	}
	out := render(&releaseInfo{Latest: "v26.10.24", Ahead: 3})
	for _, want := range []string{"<h2>Release</h2>", "Latest release <b>v26.10.24</b>; 3 commits since.", `action="/admin/release"`, "<button>Release now</button>", "No runs yet."} {
		if !strings.Contains(out, want) {
			t.Errorf("runs page missing %s", want)
		}
	}
	if out := render(&releaseInfo{Latest: "v26.10.24"}); !strings.Contains(out, "<button disabled>Release now</button>") {
		t.Errorf("Release now not disabled with nothing to release")
	}
	out = render(&releaseInfo{NoRepo: true})
	if !strings.Contains(out, "The repo isn't set up yet.") || !strings.Contains(out, "<button disabled>Release now</button>") || strings.Contains(out, "Can't read releases") {
		t.Errorf("no-commits release section wrong:\n%s", out)
	}
	if out := render(nil); strings.Contains(out, "Release now") {
		t.Errorf("Release section shown without release info")
	}
}

func TestReleaseInfoNoCommits(t *testing.T) {
	var calls int
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound) // no release, no commits/HEAD
	}))
	defer gh.Close()
	b := &Board{GitHub: github.New(gh.URL, "t"), Repo: "o/r", Releases: true}
	if ri := b.releaseInfo(context.Background()); ri == nil || !ri.NoRepo || ri.Err != "" {
		t.Fatalf("releaseInfo = %+v, want NoRepo", ri)
	}
	if calls != 2 {
		t.Fatalf("GitHub calls = %d, want 2", calls)
	}

	calls = 0
	b.Ready = func(context.Context) bool { return false }
	if ri := b.releaseInfo(context.Background()); ri == nil || !ri.NoRepo {
		t.Fatalf("releaseInfo while provisioning = %+v, want NoRepo", ri)
	}
	if calls != 0 {
		t.Fatalf("GitHub called %d times while provisioning", calls)
	}
}
