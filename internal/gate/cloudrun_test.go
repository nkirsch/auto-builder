package gate

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestServiceSHA(t *testing.T) {
	ready := `"latestReadyRevision":"projects/p/locations/r/services/x-alpha/revisions/x-alpha-2"`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/projects/p/locations/r/services/x-alpha":
			w.Write([]byte(`{` + ready + `}`))
		case "/v2/projects/p/locations/r/services/x-alpha/revisions/x-alpha-2":
			w.Write([]byte(`{"containers":[{"env":[{"name":"SITE_DIR","value":"/site"},{"name":"GIT_SHA","value":"abc"}]}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	s := &ServiceSHA{API: srv.URL, Service: "projects/p/locations/r/services/x-alpha"}
	if sha, err := s.SHA(context.Background()); err != nil || sha != "abc" {
		t.Fatalf("sha %q %v", sha, err)
	}
	ready = `"latestReadyRevision":""`
	if sha, err := s.SHA(context.Background()); err != nil || sha != "" {
		t.Fatalf("before first deploy: %q %v", sha, err)
	}
}
