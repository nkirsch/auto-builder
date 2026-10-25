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
