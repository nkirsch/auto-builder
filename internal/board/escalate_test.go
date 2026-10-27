package board

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/nkirsch/auto-builder/internal/auth"
	"github.com/nkirsch/auto-builder/internal/db"
	"github.com/nkirsch/auto-builder/internal/github"
)

type fakePlatform struct{ told []string }

func (p *fakePlatform) Admins(context.Context) ([]string, error) { return []string{"root"}, nil }
func (p *fakePlatform) Escalated(_ context.Context, it db.Item, by string) {
	p.told = append(p.told, it.Title+" by "+by)
}

func postEscalate(b *Board, m db.Member, id string, form url.Values) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "/items/"+id+"/escalate", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.SetPathValue("id", id)
	r = r.WithContext(auth.WithMember(r.Context(), m))
	w := httptest.NewRecorder()
	b.escalate(w, r)
	return w
}

func TestEscalate(t *testing.T) {
	ctx := context.Background()
	b, g := dbBoard(t)
	var calls []string
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		calls = append(calls, r.Method+" "+r.URL.Path+" "+strings.TrimSpace(string(body)))
		w.Write([]byte(`{}`))
	}))
	defer gh.Close()
	b.GitHub, b.Repo = github.New(gh.URL, "t"), "o/r"
	b.Store.UpsertMember(ctx, "nkirsch", "admin", "nkirsch")
	b.Store.UpsertMember(ctx, "kat", "admin", "nkirsch")
	b.Store.SetMemberEmail(ctx, "kat", "kat@example.org")
	b.Store.UpsertMember(ctx, "alice", "member", "nkirsch")
	// nkirsch has an email but is GitHub too, so he's still assigned by login.
	b.Store.SetMemberEmail(ctx, "nkirsch", "nick@example.org")
	b.Store.SetMemberGitHub(ctx, "nkirsch", true)
	nick, _ := b.Store.Member(ctx, "nkirsch")
	alice, _ := b.Store.Member(ctx, "alice")
	id, _ := b.Store.CreateItem(ctx, "Site down", "", "alice")
	b.Store.SetItemIssue(ctx, id, 7)
	check := func(w *httptest.ResponseRecorder, loc string) {
		t.Helper()
		if w.Code != http.StatusSeeOther || w.Header().Get("Location") != loc {
			t.Fatalf("%d %q, want 303 %q", w.Code, w.Header().Get("Location"), loc)
		}
	}

	// A member escalates to the board's admins: GitHub users get the issue, Google members are tracked by address.
	check(postEscalate(b, alice, "1", url.Values{}), "/items/1?m=Escalated%20to%20the%20admins")
	it, _ := b.Store.Item(ctx, id)
	if it.Escalation != "admin" || it.EscalatedBy != "alice" || !slices.Equal(it.Assignees, []string{"kat@example.org", "nkirsch"}) {
		t.Fatalf("item = %+v", it)
	}
	if want := []string{`POST /repos/o/r/issues/7/assignees {"assignees":["nkirsch"]}`}; !slices.Equal(calls, want) {
		t.Fatalf("GitHub calls %q, want %q", calls, want)
	}
	for _, login := range []string{"nkirsch", "kat"} {
		if n, _ := b.Store.UnreadCount(ctx, login); n != 1 {
			t.Errorf("%s unread = %d, want 1", login, n)
		}
	}
	if g.activity != 1 {
		t.Errorf("gate activity = %d", g.activity)
	}
	check(postEscalate(b, alice, "1", url.Values{}), "/items/1?m=Already%20escalated")
	if w := postEscalate(b, alice, "1", url.Values{"clear": {"true"}}); w.Code != http.StatusForbidden {
		t.Fatalf("member clear: %d", w.Code)
	}

	// An admin's same button goes to the platform admins, when there are any.
	check(postEscalate(b, nick, "1", url.Values{}), "/items/1?m=This%20board%20has%20no%20platform%20admins%20to%20escalate%20to")
	p := &fakePlatform{}
	b.Platform = p
	calls = nil
	check(postEscalate(b, nick, "1", url.Values{}), "/items/1?m=Escalated%20to%20the%20platform%20admins%20as%20urgent")
	if it, _ = b.Store.Item(ctx, id); it.Escalation != "platform" || !slices.Equal(it.Assignees, []string{"root"}) {
		t.Fatalf("item = %+v", it)
	}
	if !slices.Equal(p.told, []string{"Site down by nkirsch"}) {
		t.Errorf("platform told %q", p.told)
	}
	if want := []string{`DELETE /repos/o/r/issues/7/assignees {"assignees":["nkirsch"]}`,
		`POST /repos/o/r/issues/7/assignees {"assignees":["root"]}`}; !slices.Equal(calls, want) {
		t.Fatalf("GitHub calls %q, want %q", calls, want)
	}

	// A platform admin hands it back to one of the board's admins.
	if err := b.Reassign(ctx, id, "alice", "root"); err != ErrNotAdmin {
		t.Fatalf("reassign to member: %v", err)
	}
	if err := b.Reassign(ctx, id, "kat", "root"); err != nil {
		t.Fatal(err)
	}
	if it, _ = b.Store.Item(ctx, id); it.Escalation != "admin" || it.EscalatedBy != "root" || !slices.Equal(it.Assignees, []string{"kat@example.org"}) {
		t.Fatalf("item = %+v", it)
	}
	var n int
	b.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM notifications WHERE login = 'kat' AND kind = 'escalation' AND actor = 'root'`).Scan(&n)
	if n != 1 {
		t.Errorf("kat notified of the re-assignment %d times, want 1", n)
	}

	check(postEscalate(b, nick, "1", url.Values{"clear": {"true"}}), "/items/1?m=Escalation%20cleared")
	if it, _ = b.Store.Item(ctx, id); it.Escalation != "" {
		t.Fatalf("item = %+v", it)
	}
}

func TestEscalationShown(t *testing.T) {
	b := &Board{}
	if err := b.parse(); err != nil {
		t.Fatal(err)
	}
	render := func(name string, me db.Member, data any) string {
		var sb strings.Builder
		if err := b.tmpl[name].ExecuteTemplate(&sb, "layout", page{Title: "x", Me: me, Data: data}); err != nil {
			t.Fatal(err)
		}
		return sb.String()
	}
	admin, member := db.Member{Login: "nkirsch", Role: "admin"}, db.Member{Login: "alice", Role: "member"}
	it := db.Item{ID: 4, Title: "Site down", Status: "open", Escalation: "platform", EscalatedBy: "nkirsch",
		Assignees: []string{"root", "r@example.org"}}

	out := render("index", member, map[string]any{"Sections": sections([]db.Item{it}, false)})
	for _, want := range []string{"<h2>Escalated</h2>", `<span class="badge escalated">escalated</span> <span class="badge urgent">urgent</span>`} {
		if !strings.Contains(out, want) {
			t.Errorf("index missing %q", want)
		}
	}
	out = render("item", admin, map[string]any{"Item": it, "Repo": "o/r"})
	for _, want := range []string{"<b>Urgent:</b> escalated to the platform admins by nkirsch; assigned to root, r@example.org.",
		`<input type="hidden" name="clear" value="true">`} {
		if !strings.Contains(out, want) {
			t.Errorf("item page missing %q", want)
		}
	}
	if out = render("item", member, map[string]any{"Item": it, "Repo": "o/r"}); strings.Contains(out, `name="clear"`) {
		t.Error("member sees Clear")
	}
	it.Escalation = ""
	out = render("item", member, map[string]any{"Item": it, "Repo": "o/r", "CanEscalate": true})
	if !strings.Contains(out, `action="/items/4/escalate"`) || strings.Contains(out, "urgent") {
		t.Errorf("unescalated item page wrong:\n%s", out)
	}
}
