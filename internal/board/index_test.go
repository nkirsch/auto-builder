package board

import (
	"strings"
	"testing"

	"github.com/nkirsch/auto-builder/internal/db"
)

func TestIndexRunNowAdminOnly(t *testing.T) {
	b := &Board{}
	if err := b.parse(); err != nil {
		t.Fatal(err)
	}
	for role, want := range map[string]bool{"admin": true, "member": false} {
		var sb strings.Builder
		p := page{Title: "Board", Me: db.Member{Login: "x", Role: role}, Data: map[string]any{"Items": nil, "Show": ""}}
		if err := b.tmpl["index"].ExecuteTemplate(&sb, "layout", p); err != nil {
			t.Fatal(err)
		}
		if got := strings.Contains(sb.String(), `<form method="post" action="/admin/run">`); got != want {
			t.Errorf("%s: Run now shown = %v, want %v", role, got, want)
		}
	}
}

func TestInboxDoneToggle(t *testing.T) {
	b := &Board{}
	if err := b.parse(); err != nil {
		t.Fatal(err)
	}
	for show, link := range map[string]string{"": `<a href="/inbox?show=done">Done</a>`, "done": `<a href="/inbox">Inbox</a>`} {
		var sb strings.Builder
		p := page{Title: "Inbox", Me: db.Member{Login: "x"}, Data: map[string]any{"Notifications": nil, "Show": show}}
		if err := b.tmpl["inbox"].ExecuteTemplate(&sb, "layout", p); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(sb.String(), link) {
			t.Errorf("show=%q: missing %s", show, link)
		}
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
	p := page{Title: "Runs", Me: db.Member{Login: "x"}, Data: rows}
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

func TestInboxCollapsedCount(t *testing.T) {
	b := &Board{}
	if err := b.parse(); err != nil {
		t.Fatal(err)
	}
	ns := []db.Notification{{ItemID: 3, ItemTitle: "x", Actor: "bob", Kind: "reply", Count: 3}, {ItemID: 4, ItemTitle: "y", Count: 1}}
	var sb strings.Builder
	p := page{Title: "Inbox", Me: db.Member{Login: "x"}, Data: map[string]any{"Notifications": ns, "Show": ""}}
	if err := b.tmpl["inbox"].ExecuteTemplate(&sb, "layout", p); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(sb.String(), " updates"); n != 1 || !strings.Contains(sb.String(), "· 3 updates") {
		t.Errorf("collapsed count shown %d times", n)
	}
}

func TestIndexTags(t *testing.T) {
	b := &Board{}
	if err := b.parse(); err != nil {
		t.Fatal(err)
	}
	items := []db.Item{{ID: 8, Title: "Tags", Status: "open", Tags: []string{"feature"}}}
	var sb strings.Builder
	p := page{Title: "Board", Me: db.Member{Login: "x"}, Data: map[string]any{"Items": items, "Show": "", "Tag": "feature"}}
	if err := b.tmpl["index"].ExecuteTemplate(&sb, "layout", p); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`<a class="tag" href="/?tag=feature">#feature</a>`, `<a href="/">All tags</a>`,
		`<a href="/?show=done&amp;tag=feature">Done</a>`} {
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
	p := page{Title: "Runs", Me: db.Member{Login: "x"}, Data: []runRow{{Run: db.Run{ID: 2, Status: "done", CostUSD: &cost}}, {Run: db.Run{ID: 1, Status: "done"}}}}
	if err := b.tmpl["runs"].ExecuteTemplate(&sb, "layout", p); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(sb.String(), "$"); n != 1 || !strings.Contains(sb.String(), " · $1.23</span>") {
		t.Errorf("runs page: %d costs, want one $1.23", n)
	}

	sb.Reset()
	p = page{Title: "Admin", Me: db.Member{Login: "x", Role: "admin"}, Data: map[string]any{
		"Members": []db.Member{{Login: "x", Role: "admin"}},
		"Costs":   db.RunCosts{Total: 12.5, Last30Days: 3, Runs: 4},
	}}
	if err := b.tmpl["admin"].ExecuteTemplate(&sb, "layout", p); err != nil {
		t.Fatal(err)
	}
	if want := "Agent cost: $3.00 in the last 30 days, $12.50 in total (4 runs reported a cost)."; !strings.Contains(sb.String(), want) {
		t.Errorf("admin page missing %q", want)
	}
}
