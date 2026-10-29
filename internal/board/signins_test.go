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

// Admins see sign-in attempts, failures highlighted, and one member's alone.
func TestSignInsPage(t *testing.T) {
	ctx := context.Background()
	b, _ := dbBoard(t)
	if err := b.parse(); err != nil {
		t.Fatal(err)
	}
	b.Store.UpsertMember(ctx, "katrina", "member", "nkirsch")
	b.Store.SetMemberEmail(ctx, "katrina", "katrina@example.com")
	b.Store.UpsertMember(ctx, "octocat", "member", "nkirsch")
	for _, s := range []db.SignIn{
		{Provider: "google", Identity: "katrina@gmail.example", Result: db.SignInNotMember, IP: "203.0.113.9"},
		{Provider: "google", Identity: "katrina@example.com", Result: db.SignInUnverified},
		{Provider: "github", Identity: "octocat", Result: db.SignInOK},
	} {
		if err := b.Store.RecordSignIn(ctx, s, 0); err != nil {
			t.Fatal(err)
		}
	}
	get := func(q string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, "/admin/signins"+q, nil)
		r = r.WithContext(auth.WithMember(r.Context(), db.Member{Login: "nkirsch", Role: "admin"}))
		w := httptest.NewRecorder()
		b.signIns(w, r)
		return w
	}
	html := get("").Body.String()
	for _, want := range []string{"katrina@gmail.example", "203.0.113.9", "unverified email", "octocat", `class="failed"`} {
		if !strings.Contains(html, want) {
			t.Errorf("page missing %q", want)
		}
	}
	if strings.Contains(html, "<th>Board</th>") {
		t.Error("board column on a single board")
	}
	html = get("?who=katrina").Body.String()
	if !strings.Contains(html, "unverified email") || strings.Contains(html, "octocat") || strings.Contains(html, "gmail.example") {
		t.Errorf("katrina's page:\n%s", html)
	}
	if w := get("?who=nobody"); w.Code != http.StatusNotFound {
		t.Errorf("unknown member: %d", w.Code)
	}
}

// /admin flags members who have never signed in but failed to since being
// added, once each with their latest failure.
func TestAdminStuckSignIns(t *testing.T) {
	ctx := context.Background()
	b, _ := dbBoard(t)
	pool := b.Store.Pool
	if err := b.parse(); err != nil {
		t.Fatal(err)
	}
	b.Store.UpsertMember(ctx, "nkirsch", "admin", "nkirsch")
	b.Store.UpsertMember(ctx, "katrina", "member", "nkirsch")
	b.Store.SetMemberEmail(ctx, "katrina", "katrina@example.com")
	b.Store.UpsertMember(ctx, "octocat", "member", "nkirsch")
	b.Store.UpsertMember(ctx, "seen", "member", "nkirsch")
	b.Store.UpsertMember(ctx, "late", "member", "nkirsch")
	if _, err := pool.Exec(ctx, `UPDATE members SET last_seen = now() WHERE login = 'seen'`); err != nil {
		t.Fatal(err)
	}
	for _, s := range []db.SignIn{
		{Provider: "google", Identity: "katrina@example.com", Result: db.SignInUnverified},
		{Provider: "github", Identity: "katrina", Result: db.SignInGoogleOnly},
		{Provider: "github", Identity: "octocat", Result: db.SignInOK},
		{Provider: "github", Identity: "seen", Result: db.SignInGoogleOnly},
		{Provider: "github", Identity: "late", Result: db.SignInNotMember, Board: "other"},
		{Provider: "google", Identity: "stranger@example.com", Result: db.SignInNotMember},
	} {
		if err := b.Store.RecordSignIn(ctx, s, 0); err != nil {
			t.Fatal(err)
		}
	}
	// late's failure came before they were added.
	if _, err := pool.Exec(ctx, `UPDATE signins SET at = now() - interval '1 hour' WHERE identity = 'late';
		UPDATE signins SET at = now() - interval '2 hours' WHERE identity = 'katrina@example.com'`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE members SET added_at = now() - interval '30 minutes' WHERE login = 'late'`); err != nil {
		t.Fatal(err)
	}
	members, err := b.Store.Members(ctx)
	if err != nil {
		t.Fatal(err)
	}
	stuck, err := b.stuckSignIns(ctx, members)
	if err != nil {
		t.Fatal(err)
	}
	if len(stuck) != 1 || stuck[0].Login != "katrina" || stuck[0].Last.Result != db.SignInGoogleOnly {
		t.Fatalf("stuck = %+v, want katrina's Google-only failure alone", stuck)
	}

	r := httptest.NewRequest(http.MethodGet, "/admin", nil)
	r = r.WithContext(auth.WithMember(r.Context(), db.Member{Login: "nkirsch", Role: "admin"}))
	w := httptest.NewRecorder()
	b.admin(w, r)
	html := w.Body.String()
	for _, want := range []string{"hasn't signed in yet: signing in with GitHub as katrina failed (Google only)", `href="/admin/signins?who=katrina">Their sign-ins</a>`} {
		if !strings.Contains(html, want) {
			t.Errorf("admin page missing %q:\n%s", want, html)
		}
	}
	if strings.Contains(html, "as octocat failed") || strings.Contains(html, "as seen failed") {
		t.Error("flagged a member who signed in")
	}
}
