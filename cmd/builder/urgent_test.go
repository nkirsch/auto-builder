package main

import (
	"context"
	"net/url"
	"slices"
	"strings"
	"testing"
)

func TestHubUrgent(t *testing.T) {
	ctx := context.Background()
	h := testHub(t)
	srv := h.routes()
	if w := do(h, srv, "nkirsch", "POST", "/new", url.Values{"slug": {"alpha"}, "name": {"Alpha"}}); w.Code != 303 {
		t.Fatalf("create: %d", w.Code)
	}
	for login, role := range map[string]string{"bob": "admin", "carol": "member"} {
		do(h, srv, "nick@example.org", "POST", "/alpha/admin/members", url.Values{"login": {login}, "role": {role}})
	}
	do(h, srv, "carol", "POST", "/alpha/items", url.Values{"title": {"Login broken"}})

	// A member escalates to the board's admins; an admin's Escalate makes it urgent for the platform admins.
	if w := do(h, srv, "carol", "POST", "/alpha/items/1/escalate", url.Values{}); w.Code != 303 {
		t.Fatalf("member escalate: %d %s", w.Code, w.Body)
	}
	if w := do(h, srv, "bob", "GET", "/alpha/items/1", nil); !strings.Contains(w.Body.String(), `title="Ask the platform admins: marks it urgent"`) {
		t.Fatalf("admin has no platform Escalate:\n%s", w.Body)
	}
	if w := do(h, srv, "bob", "POST", "/alpha/items/1/escalate", url.Values{}); w.Code != 303 ||
		w.Header().Get("Location") != "/alpha/items/1?m=Escalated%20to%20the%20platform%20admins%20as%20urgent" {
		t.Fatalf("admin escalate: %d %q", w.Code, w.Header().Get("Location"))
	}
	st, _ := h.reg.Store(ctx, "alpha")
	if it, _ := st.Item(ctx, 1); it.Escalation != "platform" || !slices.Equal(it.Assignees, []string{"nkirsch"}) {
		t.Fatalf("item = %+v", it)
	}

	// The platform admins see it on Urgent, by name, and hand it back to a board admin.
	st.TouchMember(ctx, "bob", "Bob Burns", "")
	h.reg.SetUser(ctx, "nkirsch", "Nick Kirsch", "")
	if w := do(h, srv, "bob", "GET", "/admin/urgent", nil); w.Code != 403 {
		t.Fatalf("board admin on Urgent: %d", w.Code)
	}
	w := do(h, srv, "nkirsch", "GET", "/admin/urgent", nil)
	for _, want := range []string{`href="/alpha/items/1"`, `action="/admin/tenants/alpha/items/1/reassign"`,
		`escalated by <span title="Bob Burns (bob)">Bob</span>`, `<option value="bob">Bob Burns</option>`, `<summary title="Sign out…"><span title="Nick Kirsch (nkirsch)">Nick</span> ⎋</summary>`} {
		if !strings.Contains(w.Body.String(), want) {
			t.Errorf("Urgent missing %q:\n%s", want, w.Body)
		}
	}
	if w := do(h, srv, "nkirsch", "POST", "/admin/tenants/alpha/items/1/reassign", url.Values{"to": {"carol"}}); w.Header().Get("Location") !=
		"/admin/urgent?flash=%22carol%22+isn%27t+an+admin+of+alpha" {
		t.Fatalf("reassign to member: %q", w.Header().Get("Location"))
	}
	if w := do(h, srv, "nkirsch", "POST", "/admin/tenants/alpha/items/1/reassign", url.Values{"to": {"bob"}}); w.Header().Get("Location") !=
		"/admin/urgent?flash=alpha+%231+is+back+with+bob" {
		t.Fatalf("reassign: %q", w.Header().Get("Location"))
	}
	if it, _ := st.Item(ctx, 1); it.Escalation != "admin" || it.EscalatedBy != "nkirsch" || !slices.Equal(it.Assignees, []string{"bob"}) {
		t.Fatalf("item = %+v", it)
	}
	if w := do(h, srv, "nkirsch", "GET", "/admin/urgent", nil); !strings.Contains(w.Body.String(), "Nothing urgent.") {
		t.Errorf("Urgent still lists it:\n%s", w.Body)
	}
}
