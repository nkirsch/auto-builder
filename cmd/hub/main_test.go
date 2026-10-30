package main

import (
	"bytes"
	"context"
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
	t.Setenv("HUBS_CONFIG", a.URL+" env:TA # the first serves hubs\n\n"+b.URL+"/ env:TB\n")
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
		"kenko  ok      v26.10.28-1  2 unreleased  3    1     4      done",
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
