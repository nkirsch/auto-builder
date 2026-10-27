package gate

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHealthSHA(t *testing.T) {
	body := `{"status":"ok","sha":"abc123","version":"v26.10.26-1"}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(body))
	}))
	defer srv.Close()
	h := HealthSHA{URL: srv.URL + "/", Client: srv.Client()}
	if sha, err := h.SHA(context.Background()); err != nil || sha != "abc123" {
		t.Fatalf("sha %q %v", sha, err)
	}
	body = "ok v26.10.26-1"
	if _, err := h.SHA(context.Background()); err == nil {
		t.Error("plain-text /health accepted")
	}
	if _, err := (HealthSHA{URL: srv.URL + "/missing"}).SHA(context.Background()); err == nil {
		t.Error("404 accepted")
	}
}
