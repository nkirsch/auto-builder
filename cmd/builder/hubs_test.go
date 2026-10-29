package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

// /admin/hubs puts every hub's urgent, escalated and blocked items first,
// counts the rest, and lists each hub's boards; hubs read each other's
// /api/overview with STATS_TOKEN.
func TestHubOverview(t *testing.T) {
	ctx := context.Background()
	h := testHub(t)
	h.launcher, h.provisioner = &fakeJob{}, &fakeJob{}
	h.cfg.StatsToken = "tok"
	srv := h.routes()
	if w := do(h, srv, "nkirsch", "POST", "/new", url.Values{"slug": {"alpha"}, "name": {"Alpha"}}); w.Code != 303 {
		t.Fatalf("create: %d", w.Code)
	}
	st, _ := h.reg.Store(ctx, "alpha")
	urgent, _ := st.CreateItem(ctx, "Site down", "", "nkirsch")
	st.Escalate(ctx, urgent, "platform", "nkirsch", nil)
	blocked, _ := st.CreateItem(ctx, "Needs a secret", "", "nkirsch")
	st.SetBlocked(ctx, blocked, "Add the FOO secret.", false)
	done, _ := st.CreateItem(ctx, "Closed thing", "", "nkirsch")
	st.CloseItem(ctx, done, "done", true)

	// This hub's overview, as another hub reads it.
	r := httptest.NewRequest("GET", "/api/overview", nil)
	w := httptest.NewRecorder()
	if srv.ServeHTTP(w, r); w.Code != http.StatusUnauthorized {
		t.Errorf("overview without token: %d", w.Code)
	}
	r.Header.Set("Authorization", "Bearer tok")
	w = httptest.NewRecorder()
	srv.ServeHTTP(w, r)
	var o hubOverview
	if err := json.NewDecoder(w.Body).Decode(&o); err != nil || len(o.Sites) != 1 {
		t.Fatalf("overview: %d %v %+v", w.Code, err, o)
	}
	s := o.Sites[0]
	if s.Name != "Alpha" || s.Board != "https://hub.example/alpha/" || !s.App || len(s.Stats.Attention) != 2 || s.Stats.Other != 1 {
		t.Errorf("alpha overview %+v", s)
	}

	// Another hub that answers, and one that doesn't take our token.
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		json.NewEncoder(w).Encode(hubOverview{Name: "ignored", Upstream: "nkirsch/auto-builder", Behind: 3, Sites: []overviewSite{{
			Name: "Momo", Board: "https://iterate.kirsch.org/momo/", App: true, Health: "ok", Detail: true, Releases: true, Unreleased: 2,
			Stats: siteStats{Other: 4, Attention: []attention{{ID: 7, Title: "Escalated thing", Author: "katrina", Kind: "escalated"}}},
		}}})
	}))
	defer other.Close()
	locked := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer locked.Close()
	h.cfg.Hubs = []remoteSite{{Name: "kirsch", URL: other.URL}, {Name: "nick", URL: locked.URL}, {Name: "self", URL: "https://hub.example"}}

	body := do(h, srv, "nkirsch", "GET", "/admin/hubs", nil).Body.String()
	iu, ie, ib := strings.Index(body, "Site down"), strings.Index(body, "Escalated thing"), strings.Index(body, "Needs a secret")
	if iu < 0 || ie < iu || ib < ie {
		t.Errorf("attention not urgent, escalated, blocked in order (%d %d %d): %s", iu, ie, ib, body)
	}
	for _, want := range []string{
		`<a href="https://hub.example/alpha/items/` + strconv.Itoa(urgent) + `"><b>Site down</b></a>`,
		`<a href="https://iterate.kirsch.org/momo/items/7">`,
		"Add the FOO secret.",
		"5 other unfinished items",
		`<a href="` + other.URL + `/">kirsch</a>`,
		"3 commits behind nkirsch/auto-builder",
		`<span class="behind">2 unreleased</span>`,
		"HTTP 401: does it have this hub&#39;s STATS_TOKEN?",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("hubs page missing %s", want)
		}
	}
	if strings.Contains(body, "Closed thing") || strings.Count(body, `<a href="https://hub.example/">hub.example</a>`) != 1 {
		t.Errorf("hubs page shows a shipped item or this hub twice: %s", body)
	}
	if w := do(h, srv, "katrina", "GET", "/admin/hubs", nil); w.Code != http.StatusForbidden {
		t.Errorf("hubs page for a non-platform-admin: %d", w.Code)
	}
}
