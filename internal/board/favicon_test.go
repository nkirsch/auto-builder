package board

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestFaviconICO(t *testing.T) {
	b := loginBoard(t, "https://builder.example", "")
	mux := http.NewServeMux()
	if err := b.Routes(mux); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/favicon.ico", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	// ICO header: reserved 0, type 1, then a non-zero image count.
	body := w.Body.Bytes()
	if len(body) < 6 || !bytes.Equal(body[:4], []byte{0, 0, 1, 0}) || body[4] == 0 {
		t.Fatalf("not an ICO: % x", body[:min(len(body), 6)])
	}
	if ct := w.Header().Get("Content-Type"); ct != "image/vnd.microsoft.icon" && ct != "image/x-icon" {
		t.Errorf("Content-Type = %q", ct)
	}
}
