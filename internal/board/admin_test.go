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
