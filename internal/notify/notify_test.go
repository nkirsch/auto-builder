package notify

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSendsThroughSignal(t *testing.T) {
	var got []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var ev map[string]any
		json.NewDecoder(r.Body).Decode(&ev)
		if r.URL.Path != "/v1/events" || r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(400)
			return
		}
		got = append(got, ev)
		w.Write([]byte(`{"status":"delivered","delivered":true}`))
	}))
	defer srv.Close()
	t.Setenv("SIGNAL_TOKEN", "tok")
	p := New(srv.URL)
	p.Send(context.Background(), "New ask", "m", "https://x")
	p.Alert(context.Background(), "Out of credits", "m", "")
	if len(got) != 2 || got[0]["event"] != "board.notify" || got[1]["event"] != "board.alert" || got[1]["priority"] != 1.0 || got[0]["app"] != "deployments" {
		t.Fatalf("events = %v", got)
	}
}
