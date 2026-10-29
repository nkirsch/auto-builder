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
