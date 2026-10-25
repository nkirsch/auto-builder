package board

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/nkirsch/auto-builder/internal/auth"
	"github.com/nkirsch/auto-builder/internal/db"
)

func TestAutoApproveNewAsks(t *testing.T) {
	ctx := context.Background()
	b, _ := dbBoard(t)
	s := b.Store
	s.UpsertMember(ctx, "nkirsch", "admin", "test")
	s.UpsertMember(ctx, "alice", "member", "test")
	post := func(path string, m db.Member, form url.Values, h http.HandlerFunc) {
		r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r = r.WithContext(auth.WithMember(r.Context(), m))
		w := httptest.NewRecorder()
		h(w, r)
		if w.Code != http.StatusSeeOther {
			t.Fatalf("%s: status %d %s", path, w.Code, w.Body)
		}
	}
	member := func(login string) db.Member { m, _ := s.Member(ctx, login); return m }

	post("/admin/auto-approve", member("nkirsch"), url.Values{"on": {"true"}}, b.setAutoApprove)
	if !member("nkirsch").AutoApprove {
		t.Fatal("auto-approve not turned on")
	}
	// alice can't reach /admin, but even if her row said so, members don't auto-approve.
	s.SetAutoApprove(ctx, "alice", true)
	post("/items", member("nkirsch"), url.Values{"title": {"Admin ask"}}, b.createItem)
	post("/items", member("alice"), url.Values{"title": {"Member ask"}}, b.createItem)
	if it, _ := s.Item(ctx, 1); !it.Approved || it.ApprovedBy != "nkirsch" {
		t.Errorf("admin ask = %+v", it)
	}
	if it, _ := s.Item(ctx, 2); it.Approved {
		t.Errorf("member ask auto-approved")
	}

	post("/admin/auto-approve", member("nkirsch"), url.Values{"on": {"false"}}, b.setAutoApprove)
	post("/items", member("nkirsch"), url.Values{"title": {"Later ask"}}, b.createItem)
	if it, _ := s.Item(ctx, 3); it.Approved {
		t.Errorf("ask approved with auto-approve off")
	}
}

func TestAdminShowsAutoApprove(t *testing.T) {
	b := &Board{}
	if err := b.parse(); err != nil {
		t.Fatal(err)
	}
	for on, want := range map[bool]string{true: "Turn off", false: "Auto-approve my asks"} {
		var sb strings.Builder
		p := page{Title: "Admin", Me: db.Member{Login: "x", Role: "admin", AutoApprove: on}, Data: map[string]any{
			"Members": []db.Member{{Login: "x", Role: "admin"}}, "Costs": db.RunCosts{}, "Limit": 20.0,
		}}
		if err := b.tmpl["admin"].ExecuteTemplate(&sb, "layout", p); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(sb.String(), `action="/admin/auto-approve"`) || !strings.Contains(sb.String(), want) {
			t.Errorf("on=%v: missing %q", on, want)
		}
	}
}
