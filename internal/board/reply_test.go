package board

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/nkirsch/auto-builder/internal/auth"
	"github.com/nkirsch/auto-builder/internal/db"
)

type stubGate struct{ activity int }

func (g *stubGate) Activity()                                   { g.activity++ }
func (g *stubGate) RunNow(context.Context, string) (int, error) { return 0, nil }
func (g *stubGate) Cancel(context.Context, int, string) error   { return nil }

// dbBoard needs TEST_DATABASE_URL (a disposable database; tables are truncated).
func dbBoard(t *testing.T) (*Board, *stubGate) {
	t.Helper()
	u := os.Getenv("TEST_DATABASE_URL")
	if u == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	pool, err := db.Open(ctx, u)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(ctx, `TRUNCATE members, items, responses, attachments, notifications, runs, signins RESTART IDENTITY CASCADE;
		UPDATE settings SET spend_limit_usd = 20`); err != nil {
		t.Fatal(err)
	}
	g := &stubGate{}
	return &Board{Store: &db.Store{Pool: pool}, Gate: g, BaseURL: "https://builder.example", BotLogin: "builder-bot"}, g
}

func postReply(b *Board, m db.Member, id, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "/items/"+id+"/responses", strings.NewReader(url.Values{"body": {body}}.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.SetPathValue("id", id)
	r = r.WithContext(auth.WithMember(r.Context(), m))
	w := httptest.NewRecorder()
	b.reply(w, r)
	return w
}

func TestAdminReplyReopensFinishedItem(t *testing.T) {
	ctx := context.Background()
	b, g := dbBoard(t)
	s := b.Store
	admin := db.Member{Login: "nkirsch", Role: "admin"}
	member := db.Member{Login: "alice", Role: "member"}
	s.UpsertMember(ctx, admin.Login, admin.Role, "test")
	s.UpsertMember(ctx, member.Login, member.Role, "test")

	id, _ := s.CreateItem(ctx, "Dark mode", "", "alice")
	s.SetApproved(ctx, id, true, "nkirsch")
	s.SetPR(ctx, id, 4)
	s.MarkMerged(ctx, id, "abc")
	s.MarkShipped(ctx, id)

	if w := postReply(b, member, "1", "nice"); w.Header().Get("Location") != "/items/1#end" {
		t.Fatalf("member reply redirect %q", w.Header().Get("Location"))
	}
	if it, _ := s.Item(ctx, id); it.Status != "shipped" {
		t.Fatalf("member reply changed status to %s", it.Status)
	}

	w := postReply(b, admin, "1", "one more tweak please")
	if loc := w.Header().Get("Location"); loc != "/items/1?m=Reopened#end" {
		t.Fatalf("admin reply redirect %q", loc)
	}
	it, _ := s.Item(ctx, id)
	if it.Status != "open" || !it.Approved || it.PRNumber != nil {
		t.Fatalf("after admin reply: %+v", it)
	}
	if rs, _ := s.Responses(ctx, id); len(rs) != 2 {
		t.Fatalf("responses = %d", len(rs))
	}
	if gs, _ := s.GateState(ctx); !gs.NewActivity {
		t.Fatal("reopened item does not wake the gate")
	}
	if g.activity != 2 {
		t.Fatalf("gate activity = %d", g.activity)
	}
}
