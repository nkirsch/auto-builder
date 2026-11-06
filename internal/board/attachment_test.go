package board

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nkirsch/auto-builder/internal/auth"
	"github.com/nkirsch/auto-builder/internal/db"
	"github.com/nkirsch/auto-builder/internal/store"
)

// An attachment's signed link works without signing in, for its board only,
// until its item is tagged private.
func TestLinkedAttachment(t *testing.T) {
	ctx := context.Background()
	b, _ := dbBoard(t)
	dir := store.Dir(t.TempDir())
	a, err := auth.New(auth.Config{BaseURL: b.BaseURL, SessionKey: []byte("0123456789abcdef0123")}, b.Store)
	if err != nil {
		t.Fatal(err)
	}
	b.Storage, b.LinkKey, b.Auth = dir, []byte("0123456789abcdef0123"), a
	mux := http.NewServeMux()
	if err := b.Routes(mux); err != nil {
		t.Fatal(err)
	}
	id, _ := b.Store.CreateItem(ctx, "Crash", "", "alice")
	dir.Put(ctx, "attachments/1/x-shot.png", "image/png", strings.NewReader("PNG"))
	aid, _ := b.Store.AddAttachment(ctx, db.Attachment{ItemID: id, Name: "my shot [1].png", ContentType: "image/png", Size: 3,
		Object: "attachments/1/x-shot.png", UploadedBy: "alice"})
	att, _ := b.Store.Attachment(ctx, aid)

	link := b.AttachmentURL(att)
	if !strings.HasPrefix(link, "https://builder.example/attachments/1/") || !strings.HasSuffix(link, "/my_shot_1_.png") {
		t.Fatalf("link %q", link)
	}
	get := func(u string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, strings.TrimPrefix(u, "https://builder.example"), nil))
		return w
	}
	if w := get(link); w.Code != http.StatusOK || w.Body.String() != "PNG" || w.Header().Get("Content-Type") != "image/png" {
		t.Fatalf("link: %d %q %q", w.Code, w.Body, w.Header())
	}
	other := &Board{BaseURL: "https://other.example", LinkKey: b.LinkKey}
	if w := get(strings.Replace(other.AttachmentURL(att), "other", "builder", 1)); w.Code != http.StatusNotFound {
		t.Errorf("another board's link: %d", w.Code)
	}
	if w := get(strings.Replace(link, "/1/", "/2/", 1)); w.Code != http.StatusNotFound {
		t.Errorf("link for another attachment: %d", w.Code)
	}
	if w := get("/attachments/1"); w.Code == http.StatusOK {
		t.Errorf("unsigned attachment served without signing in")
	}
	b.Store.SetTags(ctx, id, []string{"private"}, false)
	if w := get(link); w.Code != http.StatusNotFound {
		t.Errorf("private item's link: %d", w.Code)
	}
	b.LinkKey = nil
	if b.AttachmentURL(att) != "" {
		t.Error("link without a key")
	}
}
