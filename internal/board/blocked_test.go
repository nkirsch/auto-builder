package board

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nkirsch/auto-builder/internal/auth"
	"github.com/nkirsch/auto-builder/internal/db"
)

func TestBlockedShown(t *testing.T) {
	b := &Board{}
	if err := b.parse(); err != nil {
		t.Fatal(err)
	}
	it := db.Item{ID: 4, Title: "Rotate key", Status: "open", Blocked: "Re-run <bootstrap>."}
	render := func(name string, me db.Member, data any) string {
		var sb strings.Builder
		if err := b.tmpl[name].ExecuteTemplate(&sb, "layout", page{Title: "x", Me: me, Data: data}); err != nil {
			t.Fatal(err)
		}
		return sb.String()
	}
	admin, member := db.Member{Login: "nkirsch", Role: "admin"}, db.Member{Login: "alice", Role: "member"}

	// Items feeds the flat list; Sections the grouped board (PR #27), whichever lands first.
	items := []db.Item{it}
	out := render("index", member, map[string]any{"Items": items, "Sections": []map[string]any{{"Items": items}}, "Show": ""})
	if !strings.Contains(out, `<span class="badge blocked" title="Re-run &lt;bootstrap&gt;.">blocked</span>`) {
		t.Errorf("index missing blocked badge:\n%s", out)
	}
	clear := `action="/items/4/unblock"`
	for _, c := range []struct {
		me        db.Member
		wantClear bool
	}{{admin, true}, {member, false}} {
		out = render("item", c.me, map[string]any{"Item": it, "Repo": "o/r"})
		if !strings.Contains(out, "<b>Blocked:</b> Re-run &lt;bootstrap&gt;.") {
			t.Errorf("%s: item page missing note", c.me.Role)
		}
		if got := strings.Contains(out, clear); got != c.wantClear {
			t.Errorf("%s: Clear shown = %v", c.me.Role, got)
		}
	}
	it.Blocked = ""
	if out = render("item", admin, map[string]any{"Item": it, "Repo": "o/r"}); strings.Contains(out, "blocked") {
		t.Errorf("unblocked item shows blocked")
	}
}

func TestUnblock(t *testing.T) {
	ctx := context.Background()
	b, g := dbBoard(t)
	id, _ := b.Store.CreateBotItem(ctx, "Needs bootstrap", "", "builder-bot")
	b.Store.SetBlocked(ctx, id, "Re-run bootstrap.", false)
	r := httptest.NewRequest(http.MethodPost, "/items/1/unblock", nil)
	r.SetPathValue("id", "1")
	r = r.WithContext(auth.WithMember(r.Context(), db.Member{Login: "nkirsch", Role: "admin"}))
	w := httptest.NewRecorder()
	b.unblock(w, r)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("status %d", w.Code)
	}
	it, _ := b.Store.Item(ctx, id)
	if it.Blocked != "" || it.HumanActivityAt.Year() < 2000 {
		t.Fatalf("item = %+v", it)
	}
	if g.activity != 1 {
		t.Errorf("gate activity = %d, want 1", g.activity)
	}
}
