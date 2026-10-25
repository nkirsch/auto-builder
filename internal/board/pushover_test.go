package board

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/nkirsch/auto-builder/internal/notify"
)

type recorder struct{ titles []string }

func (r *recorder) RoundTrip(req *http.Request) (*http.Response, error) {
	req.ParseForm()
	r.titles = append(r.titles, req.PostForm.Get("title"))
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("{}")), Header: http.Header{}}, nil
}

func TestPushoverSkipsQuietLogin(t *testing.T) {
	rec := &recorder{}
	orig := http.DefaultTransport
	http.DefaultTransport = rec
	defer func() { http.DefaultTransport = orig }()

	b := &Board{Pushover: notify.NewPushover("tok", "usr"), Quiet: "nkirsch"}
	b.pushover(context.Background(), "NKirsch", "mine", "", "")
	b.pushover(context.Background(), "bobthebooger33", "New ask from bobthebooger33", "", "")
	if len(rec.titles) != 1 || rec.titles[0] != "New ask from bobthebooger33" {
		t.Errorf("sent %q, want only the kid's ask", rec.titles)
	}
}
