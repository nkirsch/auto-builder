package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestVersionJSON(t *testing.T) {
	s := &server{version: func() string { return "v26.10.25-2" }}
	rec := httptest.NewRecorder()
	s.versionJSON(rec, httptest.NewRequest("GET", "/version", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d", rec.Code)
	}
	if got, want := rec.Body.String(), `{"version":"v26.10.25-2"}`+"\n"; got != want {
		t.Errorf("body = %q, want %q", got, want)
	}
}
