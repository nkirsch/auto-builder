package board

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/nkirsch/auto-builder/internal/auth"
	"github.com/nkirsch/auto-builder/internal/db"
)

func TestSetSpendLimit(t *testing.T) {
	ctx := context.Background()
	b, g := dbBoard(t)
	post := func(v string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/admin/spend-limit", strings.NewReader(url.Values{"limit": {v}}.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r = r.WithContext(auth.WithMember(r.Context(), db.Member{Login: "nkirsch", Role: "admin"}))
		w := httptest.NewRecorder()
		b.setSpendLimit(w, r)
		return w
	}
	for _, bad := range []string{"", "0", "-5", "abc", "NaN", "1e7"} {
		post(bad)
		if v, _ := b.Store.SpendLimit(ctx); v != 20 {
			t.Fatalf("limit %q accepted: now %v", bad, v)
		}
	}
	if w := post("$42.50"); w.Code != http.StatusSeeOther {
		t.Fatalf("status %d", w.Code)
	}
	if v, _ := b.Store.SpendLimit(ctx); v != 42.5 {
		t.Fatalf("limit = %v, want 42.5", v)
	}
	if g.activity != 1 {
		t.Errorf("gate activity = %d, want 1", g.activity)
	}
}

func TestAdminShowsRunStats(t *testing.T) {
	b := &Board{}
	if err := b.parse(); err != nil {
		t.Fatal(err)
	}
	stages := func(secs ...int) []db.RunStage {
		var out []db.RunStage
		for i, s := range secs {
			out = append(out, db.RunStage{Name: db.RunStageNames[i], State: "done", Duration: time.Duration(s) * time.Second})
		}
		return out
	}
	cpp := 1.5
	stats := []db.RunStats{
		{Runs: 3, Done: 2, Failed: 1, PRs: 2, PRsMerged: 1, Runtime: 625 * time.Second, MedianRuntime: 600 * time.Second,
			Stages: stages(10, 20, 600, 5), CostPerPR: &cpp},
		{Stages: stages(0, 0, 0, 0)},
	}
	var sb strings.Builder
	p := page{Title: "Admin", Me: db.Member{Login: "x", Role: "admin"}, Data: map[string]any{
		"Members": []db.Member{{Login: "x", Role: "admin"}}, "Costs": db.RunCosts{}, "Limit": 20.0, "Stats": stats,
	}}
	if err := b.tmpl["admin"].ExecuteTemplate(&sb, "layout", p); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"<td>Runs (done / failed)</td><td>3 (2 / 1)</td><td>0 (0 / 0)</td>",
		"<td>PRs opened (merged)</td><td>2 (1)</td><td>0 (0)</td>",
		"<td>Cost per PR</td><td>$1.50</td><td>–</td>",
		"<td>Runtime (median)</td><td>10m 25s (10m 00s)</td><td>–</td>",
		`<td class="stage">agent</td><td>10m 00s</td><td>–</td>`,
		`<td class="stage">queued</td><td>10s</td><td>–</td>`,
	} {
		if !strings.Contains(sb.String(), want) {
			t.Errorf("admin missing %s", want)
		}
	}
}

func TestUpstreamSync(t *testing.T) {
	ctx := context.Background()
	b, _ := dbBoard(t)
	b.Store.SetUpstreamSync(ctx, false)
	b.Store.SetUpstreamPR(ctx, 0)
	b.Upstream = "nkirsch/auto-builder"
	if err := b.parse(); err != nil {
		t.Fatal(err)
	}
	as := func(r *http.Request) *http.Request {
		return r.WithContext(auth.WithMember(r.Context(), db.Member{Login: "nkirsch", Role: "admin"}))
	}
	w := httptest.NewRecorder()
	b.admin(w, as(httptest.NewRequest(http.MethodGet, "/admin", nil)))
	if !strings.Contains(w.Body.String(), "Keep up with upstream") {
		t.Fatalf("no upstream switch on the admin page: %d %s", w.Code, w.Body.String())
	}
	r := httptest.NewRequest(http.MethodPost, "/admin/upstream", strings.NewReader("on=true"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	b.setUpstreamSync(httptest.NewRecorder(), as(r))
	b.Store.SetUpstreamPR(ctx, 12)
	if on, pr, _ := b.Store.UpstreamSync(ctx); !on || pr != 12 {
		t.Fatalf("on=%v pr=%d", on, pr)
	}
	w = httptest.NewRecorder()
	b.admin(w, as(httptest.NewRequest(http.MethodGet, "/admin", nil)))
	if !strings.Contains(w.Body.String(), `/pull/12">PR #12</a> is in flight`) {
		t.Errorf("admin page doesn't show the PR: %s", w.Body.String())
	}
}

func TestAdminAgentWorkflows(t *testing.T) {
	ctx := context.Background()
	b, _ := dbBoard(t)
	b.Store.Pool.Exec(ctx, `UPDATE settings SET agent_workflows = DEFAULT`)
	if err := b.parse(); err != nil {
		t.Fatal(err)
	}
	as := func(r *http.Request) *http.Request {
		return r.WithContext(auth.WithMember(r.Context(), db.Member{Login: "nkirsch", Role: "admin"}))
	}
	if on, _ := b.Store.AgentWorkflows(ctx); !on {
		t.Fatal("agent workflows off by default")
	}
	r := httptest.NewRequest(http.MethodPost, "/admin/workflows", strings.NewReader("on=false"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	b.setAgentWorkflows(httptest.NewRecorder(), as(r))
	if on, _ := b.Store.AgentWorkflows(ctx); on {
		t.Fatal("still on")
	}
	w := httptest.NewRecorder()
	b.admin(w, as(httptest.NewRequest(http.MethodGet, "/admin", nil)))
	if !strings.Contains(w.Body.String(), `<button class="secondary">Allow</button>`) {
		t.Errorf("no Allow switch: %s", w.Body.String())
	}
}

func TestAdminInviteLink(t *testing.T) {
	b := &Board{BaseURL: "https://builder.example/iterate"}
	if err := b.parse(); err != nil {
		t.Fatal(err)
	}
	seen := time.Now()
	var sb strings.Builder
	p := page{Title: "Admin", Me: db.Member{Login: "x", Role: "admin"}, Data: map[string]any{
		"Members": []db.Member{{Login: "x", Role: "admin", LastSeen: &seen}, {Login: "newbie", Role: "member"},
			{Login: "g", Role: "member", Email: "g@example.org"}},
		"Costs": db.RunCosts{}, "Limit": 20.0,
	}}
	if err := b.tmpl["admin"].ExecuteTemplate(&sb, "layout", p); err != nil {
		t.Fatal(err)
	}
	html := sb.String()
	if n := strings.Count(html, ">Invite</a>"); n != 2 {
		t.Errorf("%d Invite links, want 2 (members who never signed in)", n)
	}
	for _, want := range []string{
		`href="mailto:?body=I%20added%20you%20to%20the%20builder.example%20board`,
		`%0A%0Ahttps%3A%2F%2Fbuilder.example%2Fiterate%2F%0A%0ASign%20in%20with%20GitHub%20as%20newbie.`,
		`href="mailto:g@example.org?body=`,
		`Sign%20in%20with%20Google%20as%20g%40example.org.`,
		`subject=You%27re%20on%20the%20builder.example%20board`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("admin missing %s", want)
		}
	}
	if t.Failed() {
		i := strings.Index(html, "mailto:")
		t.Logf("%.600s", html[i:])
	}
}

type stubWelcomer struct {
	names, logins []string
	err           error
}

func (s *stubWelcomer) Welcome(_ context.Context, name, login string) (int, error) {
	s.names, s.logins = append(s.names, name), append(s.logins, login)
	return 7, s.err
}

// A new member without an email is welcomed on GitHub; members with an
// email (GitHub too or not) get an email invite instead, and existing ones nothing.
func TestAddMemberWelcomesOnGitHub(t *testing.T) {
	b, _ := dbBoard(t)
	wel := &stubWelcomer{}
	b.Welcomer = wel
	add := func(login, email string, github ...string) string {
		r := httptest.NewRequest(http.MethodPost, "/admin/members", strings.NewReader(url.Values{
			"login": {login}, "role": {"member"}, "email": {email}, "github": github}.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r = r.WithContext(auth.WithMember(r.Context(), db.Member{Login: "nkirsch", Role: "admin"}))
		w := httptest.NewRecorder()
		b.addMember(w, r)
		loc, _ := url.Parse(w.Header().Get("Location"))
		return loc.Query().Get("m")
	}
	if got := add("@newbie", ""); got != "Saved newbie and welcomed them on GitHub issue #7" {
		t.Errorf("flash %q", got)
	}
	add("newbie", "")
	add("gmail", "g@example.org")
	if len(wel.logins) != 1 || wel.logins[0] != "newbie" || wel.names[0] != "builder.example" {
		t.Fatalf("welcomed %v as %v", wel.logins, wel.names)
	}
	// An email plus "GitHub too": the email invite reaches them, so no public mention.
	if got := add("setheck", "setheck@example.org", "on"); got != "Saved setheck" {
		t.Errorf("flash %q", got)
	}
	if m, _ := b.Store.Member(context.Background(), "setheck"); !m.GitHub || m.Email != "setheck@example.org" {
		t.Errorf("setheck = %+v", m)
	}
	if len(wel.logins) != 1 {
		t.Fatalf("welcomed %v", wel.logins)
	}
	wel.err = errors.New("boom")
	if got := add("other", ""); !strings.Contains(got, "couldn't welcome them") {
		t.Errorf("flash %q", got)
	}
	if _, err := b.Store.Member(context.Background(), "other"); err != nil {
		t.Errorf("member not saved when the welcome failed: %v", err)
	}
}

// Admins mark a member with an email as GitHub too, or back to Google only.
func TestSetMemberGitHub(t *testing.T) {
	ctx := context.Background()
	b, _ := dbBoard(t)
	b.Store.UpsertMember(ctx, "setheck", "member", "nkirsch")
	b.Store.SetMemberEmail(ctx, "setheck", "setheck@example.org")
	set := func(login, on string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/admin/members/"+login+"/github", strings.NewReader(url.Values{"on": {on}}.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.SetPathValue("login", login)
		r = r.WithContext(auth.WithMember(r.Context(), db.Member{Login: "nkirsch", Role: "admin"}))
		w := httptest.NewRecorder()
		b.setMemberGitHub(w, r)
		return w
	}
	if w := set("setheck", "true"); w.Code != http.StatusSeeOther {
		t.Fatalf("status %d", w.Code)
	}
	if m, _ := b.Store.Member(ctx, "setheck"); !m.GitHub || !m.GitHubSignIn() {
		t.Fatalf("not GitHub too: %+v", m)
	}
	set("setheck", "false")
	if m, _ := b.Store.Member(ctx, "setheck"); m.GitHub || m.GitHubSignIn() {
		t.Fatalf("still GitHub too: %+v", m)
	}
	if w := set("nobody", "true"); w.Code != http.StatusNotFound {
		t.Errorf("unknown member: %d", w.Code)
	}
}

type stubMailer struct {
	from     string
	to, body []string
	err      error
}

func (s *stubMailer) Mailbox(*http.Request) string { return s.from }

func (s *stubMailer) Mail(_ *http.Request, to, subject, body string) (string, error) {
	s.to, s.body = append(s.to, to), append(s.body, subject+"\n"+body)
	return s.from, s.err
}

// With Gmail connected, members with an email get an Email invite button;
// those without keep the mailto link.
func TestAdminEmailInvite(t *testing.T) {
	b := &Board{BaseURL: "https://builder.example"}
	if err := b.parse(); err != nil {
		t.Fatal(err)
	}
	render := func(mail map[string]any) string {
		var sb strings.Builder
		p := page{Title: "Admin", Me: db.Member{Login: "x", Role: "admin"}, Data: map[string]any{
			"Members": []db.Member{{Login: "newbie", Role: "member"}, {Login: "g", Role: "member", Email: "g@example.org"}},
			"Costs":   db.RunCosts{}, "Limit": 20.0, "Mail": mail,
		}}
		if err := b.tmpl["admin"].ExecuteTemplate(&sb, "layout", p); err != nil {
			t.Fatal(err)
		}
		return sb.String()
	}
	html := render(map[string]any{"From": "nick@example.com"})
	if !strings.Contains(html, `action="/admin/members/g/invite"`) || strings.Contains(html, `mailto:g@example.org`) {
		t.Error("g should get an Email invite button, not a mailto link")
	}
	if !strings.Contains(html, `href="mailto:?`) || !strings.Contains(html, "Invites go from nick@example.com") {
		t.Error("newbie keeps the mailto link; sender shown")
	}
	html = render(map[string]any{"From": ""})
	if strings.Contains(html, "/invite\"") || !strings.Contains(html, `href="/auth/gmail?next=/admin">Connect Gmail`) {
		t.Error("unconnected: mailto links and a Connect Gmail link")
	}
}

func TestSendInvite(t *testing.T) {
	ctx := context.Background()
	b, _ := dbBoard(t)
	b.Store.UpsertMember(ctx, "g", "member", "nkirsch")
	b.Store.SetMemberEmail(ctx, "g", "g@example.org")
	b.Store.UpsertMember(ctx, "newbie", "member", "nkirsch")
	mail := &stubMailer{from: "nick@example.com"}
	b.Mailer = mail
	post := func(login string) string {
		r := httptest.NewRequest(http.MethodPost, "/admin/members/"+login+"/invite", nil)
		r.SetPathValue("login", login)
		r = r.WithContext(auth.WithMember(r.Context(), db.Member{Login: "nkirsch", Role: "admin"}))
		w := httptest.NewRecorder()
		b.sendInvite(w, r)
		loc, _ := url.Parse(w.Header().Get("Location"))
		return loc.Query().Get("m")
	}
	if f := post("g"); !strings.Contains(f, "Emailed an invite to g@example.org from nick@example.com") {
		t.Errorf("flash %q", f)
	}
	if len(mail.to) != 1 || mail.to[0] != "g@example.org" || !strings.Contains(mail.body[0], "Sign in with Google as g@example.org.") {
		t.Errorf("sent %v %q", mail.to, mail.body)
	}
	if f := post("newbie"); !strings.Contains(f, "no email") || len(mail.to) != 1 {
		t.Errorf("newbie: flash %q, %d sent", f, len(mail.to))
	}
	mail.err = errors.New("invalid_grant")
	if f := post("g"); !strings.Contains(f, "Couldn't email g@example.org") {
		t.Errorf("failure flash %q", f)
	}
}

func TestAdminLinksTraffic(t *testing.T) {
	b := &Board{}
	if err := b.parse(); err != nil {
		t.Fatal(err)
	}
	for _, on := range []bool{false, true} {
		var sb strings.Builder
		p := page{Title: "Admin", Me: db.Member{Login: "x", Role: "admin"}, Data: map[string]any{
			"Members": []db.Member{{Login: "x", Role: "admin"}}, "Costs": db.RunCosts{}, "Limit": 20.0, "Traffic": on,
		}}
		if err := b.tmpl["admin"].ExecuteTemplate(&sb, "layout", p); err != nil {
			t.Fatal(err)
		}
		if got := strings.Contains(sb.String(), `href="/logs">Site traffic</a>`); got != on {
			t.Errorf("Traffic=%v: link shown=%v", on, got)
		}
	}
}

// With GitHub sign-in off, the admin page drops GitHub choices and requires an email.
func TestAdminGitHubSignInOff(t *testing.T) {
	b := &Board{BaseURL: "https://builder.example"}
	if err := b.parse(); err != nil {
		t.Fatal(err)
	}
	for _, on := range []bool{true, false} {
		var sb strings.Builder
		p := page{Title: "Admin", Me: db.Member{Login: "x", Role: "admin"}, Data: map[string]any{
			"Members": []db.Member{{Login: "g", Role: "member", Email: "g@example.org", GitHub: true}},
			"Costs":   db.RunCosts{}, "Limit": 20.0, "GitHub": on,
		}}
		if err := b.tmpl["admin"].ExecuteTemplate(&sb, "layout", p); err != nil {
			t.Fatal(err)
		}
		html := sb.String()
		for _, s := range []string{"GitHub too", " or GitHub", "Google only"} {
			if strings.Contains(html, s) != on {
				t.Errorf("GitHub sign-in %v: %q shown %v", on, s, !on)
			}
		}
		if strings.Contains(html, `placeholder="Google email" required`) == on {
			t.Errorf("GitHub sign-in %v: email required %v", on, on)
		}
	}
}

func TestWho(t *testing.T) {
	names := map[string]string{"nkirsch": "Nick Kirsch", "kat": "", "x": `<b>"Al"</b> Smith`}
	for login, want := range map[string]string{
		"nkirsch":  `<span title="Nick Kirsch (nkirsch)">Nick</span>`,
		"kat":      "kat",
		"stranger": "stranger",
		"x":        `<span title="&lt;b&gt;&#34;Al&#34;&lt;/b&gt; Smith (x)">&lt;b&gt;&#34;Al&#34;&lt;/b&gt;</span>`,
	} {
		if got := string(who(names, login)); got != want {
			t.Errorf("who(%q) = %s, want %s", login, got, want)
		}
	}
}
