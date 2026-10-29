package board

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/nkirsch/auto-builder/internal/db"
	"github.com/nkirsch/auto-builder/internal/gate"
	"github.com/nkirsch/auto-builder/internal/github"
)

// An admin reply on a merged item reopens it once it ships, not before, so
// the ship-checker still sees the merge.
func TestAdminReplyOnMergedReopensOnShip(t *testing.T) {
	ctx := context.Background()
	b, _ := dbBoard(t)
	s := b.Store
	admin := db.Member{Login: "nkirsch", Role: "admin"}
	merged := func(title string) string {
		id, _ := s.CreateItem(ctx, title, "x", "alice")
		s.SetApproved(ctx, id, true, "nkirsch")
		s.SetPR(ctx, id, 7)
		s.MarkMerged(ctx, id, "abc123")
		return strconv.Itoa(id)
	}
	followUp, member := merged("Dark mode"), merged("Light mode")

	w := postReply(b, admin, followUp, "also the footer, please")
	if loc, _ := url.Parse(w.Header().Get("Location")); loc.Query().Get("m") != "Reopens once shipped" {
		t.Errorf("flash %q", loc.Query().Get("m"))
	}
	postReply(b, db.Member{Login: "alice", Role: "member"}, member, "thanks!")
	if it, _ := s.Item(ctx, 1); it.Status != "merged" || it.MergeSHA != "abc123" {
		t.Fatalf("after the reply: %s %q", it.Status, it.MergeSHA)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/repos/o/r/compare/"):
			w.Write([]byte(`{"status":"behind"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	var notes []string
	ship := &gate.Shipper{Store: s, GitHub: github.New(srv.URL, ""), Repo: "o/r", GitSHA: "live",
		Reply: func(_ context.Context, it db.Item, body string) error {
			notes = append(notes, strconv.Itoa(it.ID)+": "+body)
			return nil
		}}
	if err := ship.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if it, _ := s.Item(ctx, 1); it.Status != "open" || it.PRNumber != nil || !it.Approved {
		t.Errorf("follow-up item: %s pr %v approved %v", it.Status, it.PRNumber, it.Approved)
	}
	if it, _ := s.Item(ctx, 2); it.Status != "shipped" {
		t.Errorf("member-replied item: %s", it.Status)
	}
	slices.Sort(notes)
	want := []string{"1: Shipped in abc123. Reopened for the follow-up above.", "2: Shipped in abc123."}
	if strings.Join(notes, "\n") != strings.Join(want, "\n") {
		t.Errorf("notes:\n%s", strings.Join(notes, "\n"))
	}
	// Shipping it again later doesn't reopen it a second time.
	s.SetPR(ctx, 1, 8)
	s.MarkMerged(ctx, 1, "def456")
	ship.Check(ctx)
	if it, _ := s.Item(ctx, 1); it.Status != "shipped" {
		t.Errorf("second ship: %s", it.Status)
	}
}
