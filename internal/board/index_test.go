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
