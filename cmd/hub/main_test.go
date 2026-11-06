package main

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakeHub answers the tenant API for its boards, behind token.
func fakeHub(t *testing.T, token string, boards ...string) (*httptest.Server, *[]string) {
	t.Helper()
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		body, _ := io.ReadAll(r.Body)
		calls = append(calls, r.Method+" "+r.URL.Path+" "+string(body))
		slug := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/tenants/"), "/")[0]
		known := false
		for _, b := range boards {
			known = known || b == slug
		}
		switch {
		case r.URL.Path == "/api/hubs":
			w.Write([]byte(`{"hubs":[{"name":"extreme.org","url":"https://extreme.org","version":"v26.10.29-1","release_known":true,
				"sites":[{"name":"kenko","app":true,"health":"ok","detail":true,"releases":true,"unreleased":2,"errors_known":true,"errors_24h":3,
				"stats":{"version":"v26.10.28-1","attention":[{}],"other":4,"last_run":{"status":"done","at":"2026-10-29T10:00:00Z"}}}]},
				{"name":"family","url":"https://hub.example.net","error":"HTTP 401"}],
				"attention":[{"id":7,"title":"Site down","author":"nkirsch","kind":"urgent","hub":"extreme.org","site":"kenko","url":"https://extreme.org/kenko/items/7","at":"2026-10-29T11:00:00Z"}],
				"other":4}`))
		case !known:
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"error":"no tenant"}`))
		case r.Method == "GET":
			w.Write([]byte(`{"slug":"` + slug + `"}`))
		case strings.HasSuffix(r.URL.Path, "/items"):
			var in map[string]any
			json.Unmarshal(body, &in)
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(map[string]any{"id": 12, "url": "https://hub/" + slug + "/items/12", "approved": in["approve"]})
		case strings.HasSuffix(r.URL.Path, "/run") && slug == "busy":
			w.WriteHeader(http.StatusConflict)
			w.Write([]byte(`{"error":"a run is already active"}`))
		case strings.HasSuffix(r.URL.Path, "/run"):
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(`{"run":5,"runs":"https://hub/` + slug + `/runs"}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func TestCLI(t *testing.T) {
	ctx := context.Background()
	a, aCalls := fakeHub(t, "ta", "maple")
	b, bCalls := fakeHub(t, "tb", "kenko", "busy")
	t.Setenv("TA", "ta")
	t.Setenv("TB", "tb")
	t.Setenv("HUBS_CONFIG", a.URL+" env:TA # a comment\n\n"+b.URL+"/ env:TB\n")
	hub := func(args ...string) (string, error) {
		var out bytes.Buffer
		err := run(ctx, args, &out)
		return out.String(), err
	}

	// add finds the board's hub, files it approved, and with -run starts a run.
	out, err := hub("add", "kenko", "kenko should group by hub", "-run")
	if err != nil || out != "#12 https://hub/kenko/items/12 (approved)\nrun 5 started: https://hub/kenko/runs\n" {
		t.Fatalf("add -run: %q %v", out, err)
	}
	if got := (*bCalls)[1]; got != `POST /api/tenants/kenko/items {"approve":true,"title":"kenko should group by hub"}` {
		t.Errorf("file call: %s", got)
	}
	if len(*aCalls) != 1 {
		t.Errorf("hub a got more than the lookup: %v", *aCalls)
	}
	out, err = hub("add", "-draft", "maple", "Title", "Body text")
	if err != nil || out != "#12 https://hub/maple/items/12 (unapproved)\n" {
		t.Errorf("add -draft: %q %v", out, err)
	}
	if _, err := hub("run", "nope"); err == nil || !strings.Contains(err.Error(), "no hub has a board nope") {
		t.Errorf("missing board: %v", err)
	}
	if _, err := hub("run", "busy"); err == nil || err.Error() != "not started: a run is already active" {
		t.Errorf("busy: %v", err)
	}
	if _, err := hub("add", "kenko"); err == nil || !strings.Contains(err.Error(), "usage") {
		t.Errorf("no title: %v", err)
	}

	// A hub that refuses the token is reported, not taken for a missing board.
	t.Setenv("TA", "wrong")
	if _, err := hub("run", "maple"); err == nil || !strings.Contains(err.Error(), "HTTP 401") {
		t.Errorf("bad token: %v", err)
	}
	t.Setenv("TA", "ta")

	out, err = hub()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"urgent  Site down  extreme.org · kenko · #7 by nkirsch",
		"4 other unfinished items",
		"EXTREME.ORG V26.10.29-1 · RELEASED  https://extreme.org",
		"kenko  ok      v26.10.28-1 +2  3    1     4      done",
		"couldn't read https://hub.example.net/api/overview: HTTP 401",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("hubs missing %q:\n%s", want, out)
		}
	}
	if out, err := hub("-json"); err != nil || !json.Valid([]byte(out)) {
		t.Errorf("hubs -json: %v %q", err, out)
	}
}

// overviewHub answers GET /api/hubs with body.
func overviewHub(t *testing.T, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestOverviewAllHubs(t *testing.T) {
	ctx := context.Background()
	a := overviewHub(t, `{"hubs":[{"name":"nick.org","url":"https://iterate.nick.org","release_known":true,
		"sites":[{"name":"kenko","app":true,"health":"ok","detail":true,"releases":true,"unreleased":0,"site":"https://kenko.nick.org",
		"stats":{"version":"v1","other":2,"attention":[{}],"last_run":{"status":"done","at":"2026-11-06T10:00:00Z"}}}]},
		{"name":"kirsch.org","url":"https://iterate.kirsch.org","error":"peer down"}],
		"attention":[{"id":1,"title":"Down","kind":"urgent","hub":"nick.org","site":"kenko","url":"https://iterate.nick.org/kenko/items/1"}],"other":2}`)
	b := overviewHub(t, `{"hubs":[{"name":"kirsch.org","url":"https://iterate.kirsch.org",
		"sites":[{"name":"momo","app":true,"health":"ok,\"odd\"","detail":true,"stats":{"sha":"abcdef123456","other":3}}]},
		{"name":"sorted","url":"https://sorted.example","sites":[{"name":"zebra"},{"name":"Banana"},{"name":"apple"}]}],"other":3}`)
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
	}))
	t.Cleanup(down.Close)
	t.Setenv("T", "x")
	t.Setenv("HUBS_CONFIG", a.URL+" env:T\n"+b.URL+" env:T\n"+down.URL+" env:T\n")
	hub := func(args ...string) (string, error) {
		var out bytes.Buffer
		err := run(ctx, args, &out)
		return out.String(), err
	}

	out, err := hub()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Index(out, "apple") > strings.Index(out, "Banana") || strings.Index(out, "Banana") > strings.Index(out, "zebra") {
		t.Errorf("boards not sorted ignoring case:\n%s", out)
	}
	for _, want := range []string{"── NICK.ORG", "── KIRSCH.ORG", "momo", "HTTP 401", "5 other unfinished items"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "peer down") {
		t.Errorf("a peer's error should give way to the hub's own answer:\n%s", out)
	}

	out, err = hub("-csv")
	if err != nil {
		t.Fatal(err)
	}
	rows, err := csv.NewReader(strings.NewReader(out)).ReadAll()
	if err != nil {
		t.Fatalf("csv: %v\n%s", err, out)
	}
	if len(rows) != 7 || rows[0][0] != "hub" || rows[1][0] != "nick.org" || rows[1][1] != "kenko" ||
		rows[2][4] != "abcdef1" || rows[2][2] != `ok,"odd"` || rows[3][1] != "apple" || !strings.Contains(rows[6][15], "HTTP 401") {
		t.Errorf("csv rows: %q", rows)
	}

	var merged struct {
		Hubs  []json.RawMessage `json:"hubs"`
		Other int               `json:"other"`
	}
	out, err = hub("-json")
	if err != nil || json.Unmarshal([]byte(out), &merged) != nil || len(merged.Hubs) != 4 || merged.Other != 5 {
		t.Errorf("-json: %v %q", err, out)
	}

	// -hub matches the config URL, and drops peers and attention it doesn't match.
	t.Setenv("HUBS_CONFIG", "https://iterate.nick.org.invalid env:T\n")
	if _, err := hub("-hub", "nope"); err == nil || !strings.Contains(err.Error(), "no hub matches") {
		t.Errorf("-hub nope: %v", err)
	}
	t.Setenv("HUBS_CONFIG", a.URL+" env:T\n"+b.URL+" env:T\n")
	out, err = hub("-hub", "127.0.0.1,kirsch")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "NICK.ORG") || !strings.Contains(out, "KIRSCH.ORG") || strings.Contains(out, "Down") {
		t.Errorf("-hub kirsch:\n%s", out)
	}
	if _, err := hub("-json", "-csv"); err == nil {
		t.Error("-json -csv should be refused")
	}
}

func TestVersionCell(t *testing.T) {
	site := func(version string, releases bool, unreleased int) hubSite {
		var s hubSite
		s.Stats.Version, s.Releases, s.Unreleased = version, releases, unreleased
		return s
	}
	for _, c := range []struct {
		site hubSite
		want string
	}{
		{site("v26.11.6-1", true, 0), "v26.11.6-1"},
		{site("v26.11.6-1", true, 4), "v26.11.6-1 +4"},
		{site("v26.11.6-1-2-gba6e69f", true, 2), "v26.11.6-1 +2"},
		{site("v26.11.2-2-1-gf04b820", true, 0), "v26.11.2-2-1-gf04b820"},
		{site("", false, 0), "–"},
	} {
		if got := versionCell(c.site); got != c.want {
			t.Errorf("versionCell(%+v) = %q, want %q", c.site.Stats.Version, got, c.want)
		}
	}
}

func TestParseHubs(t *testing.T) {
	h, err := parseHubs("https://extreme.org nkirsch-auto-builder extreme-ADMIN_TOKEN\nhttps://hub.example.net/ env:K\n")
	if err != nil || len(h) != 2 || h[0].Secret != "extreme-ADMIN_TOKEN" || h[1].URL != "https://hub.example.net" || h[1].Env != "K" {
		t.Errorf("%+v %v", h, err)
	}
	if _, err := parseHubs("https://x.org only-one"); err == nil {
		t.Error("bad line accepted")
	}
}

func TestAgo(t *testing.T) {
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	for d, want := range map[time.Duration]string{30 * time.Second: "just now", 5 * time.Minute: "5m ago", 3 * time.Hour: "3h ago", 72 * time.Hour: "3d ago"} {
		if got := ago(now, now.Add(-d)); got != want {
			t.Errorf("ago(%v) = %q, want %q", d, got, want)
		}
	}
}
