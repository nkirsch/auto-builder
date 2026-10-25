package board

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/nkirsch/auto-builder/internal/auth"
	"github.com/nkirsch/auto-builder/internal/db"
)

func TestBoardUnreadMarkers(t *testing.T) {
	ctx := context.Background()
	b, _ := dbBoard(t)
	if err := b.parse(); err != nil {
		t.Fatal(err)
	}
	s := b.Store
	alice := db.Member{Login: "alice", Role: "member"}
	s.UpsertMember(ctx, alice.Login, alice.Role, "test")
	id, _ := s.CreateItem(ctx, "Mine", "", "alice")
	s.CreateItem(ctx, "Other", "", "bob")
	s.Notify(ctx, "alice", id, "bob", "reply")

	get := func() string {
		r := httptest.NewRequest(http.MethodGet, "/", nil).WithContext(auth.WithMember(ctx, alice))
		w := httptest.NewRecorder()
		b.index(w, r)
		return w.Body.String()
	}
	out := get()
	if n := strings.Count(out, `class="unread"`); n != 1 || !strings.Contains(out, `title="Unread updates">
    <a href="/items/`+strconv.Itoa(id)+`"`) {
		t.Fatalf("unread markers = %d, want item %d only", n, id)
	}

	r := httptest.NewRequest(http.MethodPost, "/read", strings.NewReader(url.Values{"back": {"/?tag=bug"}}.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	b.markAllRead(w, r.WithContext(auth.WithMember(ctx, alice)))
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/?tag=bug" {
		t.Fatalf("mark all read = %d %s", w.Code, w.Header().Get("Location"))
	}
	if out := get(); strings.Contains(out, `class="unread"`) {
		t.Errorf("unread marker left after mark all read")
	}
}
