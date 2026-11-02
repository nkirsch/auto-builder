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

func TestRevisions(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/projects/p/locations/r/services/x-alpha/revisions" {
			http.NotFound(w, r)
			return
		}
		if r.URL.Query().Get("pageToken") == "" {
			w.Write([]byte(`{"revisions":[{"name":"projects/p/locations/r/services/x-alpha/revisions/x-alpha-2","createTime":"2026-10-31T12:00:00Z",
				"containers":[{"env":[{"name":"GIT_SHA","value":"abc"}]}]}],"nextPageToken":"n"}`))
			return
		}
		w.Write([]byte(`{"revisions":[{"name":"projects/p/locations/r/services/x-alpha/revisions/x-alpha-1","createTime":"2026-10-01T12:00:00Z"}]}`))
	}))
	defer srv.Close()
	s := &ServiceSHA{API: srv.URL, Service: "projects/p/locations/r/services/x-alpha"}
	revs, err := s.Revisions(context.Background(), 5)
	if err != nil || len(revs) != 2 || revs[0].Name != "x-alpha-2" || revs[0].SHA != "abc" || revs[1].SHA != "" || revs[0].Created.Day() != 31 {
		t.Fatalf("revisions %+v %v", revs, err)
	}
}
