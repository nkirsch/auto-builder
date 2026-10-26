package board

import (
	"context"
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
