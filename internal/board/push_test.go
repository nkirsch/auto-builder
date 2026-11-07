package board

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/nkirsch/auto-builder/internal/notify"
)

func TestPushSkipsQuietLogin(t *testing.T) {
	var titles []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var ev struct {
			Title string `json:"title"`
		}
		json.NewDecoder(r.Body).Decode(&ev)
		titles = append(titles, ev.Title)
		w.Write([]byte(`{"status":"delivered","delivered":true}`))
	}))
	defer srv.Close()
	t.Setenv("SIGNAL_TOKEN", "tok")

	b := &Board{Push: notify.New(srv.URL), Quiet: "nkirsch"}
	b.push(context.Background(), "NKirsch", "mine", "", "")
	b.push(context.Background(), "bobthebooger33", "New ask from bobthebooger33", "", "")
	if len(titles) != 1 || titles[0] != "New ask from bobthebooger33" {
		t.Errorf("sent %q, want only the kid's ask", titles)
	}
}
