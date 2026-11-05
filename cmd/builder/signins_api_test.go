package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/nkirsch/auto-builder/internal/auth"
	"github.com/nkirsch/auto-builder/internal/db"
)

// A single board lists its sign-ins and its sessions' later IPs + browsers.
func TestBoardSignIns(t *testing.T) {
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
	if _, err := pool.Exec(ctx, `TRUNCATE signins, seen`); err != nil {
		t.Fatal(err)
	}
	st := &db.Store{Pool: pool}
	s := &server{store: st}
	for _, in := range []db.SignIn{
		{Provider: "google", Identity: "pat@example.com", Result: db.SignInOK, IP: "1.2.3.4", UserAgent: "Safari"},
		{Provider: "google", Identity: "eve@example.com", Result: db.SignInNotMember, IP: "6.6.6.6", UserAgent: "x"},
		{Provider: "google", Identity: "unverified@example.com", Result: "unverified email", IP: "7.7.7.7", UserAgent: "x"},
	} {
		if err := st.RecordSignIn(ctx, in, 0); err != nil {
			t.Fatal(err)
		}
	}

	var seen seenNoter
	page := seen.wrap(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}),
		func(*http.Request) string { return "pat@example.com" }, s.recordSeen)
	for _, path := range []string{"/runs", "/runs", "/static/style.css"} {
		r := httptest.NewRequest("GET", path, nil)
		r.Header.Set("X-Forwarded-For", "9.9.9.9")
		r.Header.Set("User-Agent", "Phone")
		if path == "/static/style.css" {
			r.Header.Set("X-Forwarded-For", "8.8.8.8")
		}
		page.ServeHTTP(httptest.NewRecorder(), r)
	}

	h := auth.Bearer("tok", s.signIns())
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/api/signins", nil))
	if w.Code != http.StatusUnauthorized {
		t.Errorf("no token: %d", w.Code)
	}
	r := httptest.NewRequest("GET", "/api/signins?days=7", nil)
	r.Header.Set("Authorization", "Bearer tok")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	var out []struct{ Identity, IP, UA string }
	if err := json.NewDecoder(w.Body).Decode(&out); err != nil || w.Code != http.StatusOK {
		t.Fatalf("%d %v", w.Code, err)
	}
	if len(out) != 2 || out[0].IP != "1.2.3.4" || out[1].IP != "9.9.9.9" || out[1].UA != "Phone" || out[1].Identity != "pat@example.com" {
		t.Errorf("signins: %+v", out)
	}

	r = httptest.NewRequest("GET", "/api/signins?days=7&failed=1", nil)
	r.Header.Set("Authorization", "Bearer tok")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	var all []struct {
		Identity string
		Refused  bool
	}
	if err := json.NewDecoder(w.Body).Decode(&all); err != nil || w.Code != http.StatusOK {
		t.Fatalf("%d %v", w.Code, err)
	}
	if len(all) != 3 || all[0].Identity != "eve@example.com" || !all[0].Refused || all[1].Refused || all[2].Refused {
		t.Errorf("with failed: %+v", all)
	}
}
