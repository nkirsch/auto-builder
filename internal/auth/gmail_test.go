package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeGmail serves Google's token, userinfo and Gmail send endpoints,
// keeping each sent message's raw RFC 5322 text.
func fakeGmail(t *testing.T, sent *[]string) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/token":
			r.ParseForm()
			if r.Form.Get("grant_type") == "refresh_token" && r.Form.Get("refresh_token") != "refresh-1" {
				http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"access_token": "tok", "token_type": "Bearer",
				"refresh_token": "refresh-1", "expires_in": 3600})
		case "/userinfo":
			json.NewEncoder(w).Encode(map[string]any{"email": "Nick@Example.com", "email_verified": true})
		case "/gmail/v1/users/me/messages/send":
			if r.Header.Get("Authorization") != "Bearer tok" {
				http.Error(w, "no token", http.StatusUnauthorized)
				return
			}
			var m struct{ Raw string }
			json.NewDecoder(r.Body).Decode(&m)
			raw, _ := base64.RawURLEncoding.DecodeString(m.Raw)
			*sent = append(*sent, string(raw))
			json.NewEncoder(w).Encode(map[string]string{"id": "m1"})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// An admin connects Gmail; the refresh token is stored sealed and mail
// goes out from their address.
func TestGmailConnectAndMail(t *testing.T) {
	ctx := context.Background()
	a, st := dbAuth(t)
	st.UpsertMember(ctx, "nkirsch", "admin", "test")
	var sent []string
	srv := fakeGmail(t, &sent)
	a.gmail.Endpoint.TokenURL = srv.URL + "/token"
	a.userinfo = srv.URL + "/userinfo"
	a.gmailAPI = srv.URL

	signedIn := func(r *http.Request) *http.Request {
		r.AddCookie(a.SessionCookie("nkirsch"))
		return r
	}
	if from, err := a.Mail(signedIn(httptest.NewRequest(http.MethodPost, "/", nil)), "k@example.org", "s", "b"); err != ErrNoMailbox {
		t.Fatalf("unconnected Mail = %q, %v", from, err)
	}

	r := signedIn(httptest.NewRequest(http.MethodGet, "/auth/gmail/callback?state=abc&code=c", nil))
	r.AddCookie(&http.Cookie{Name: stateCookie, Value: "abc|/admin"})
	w := httptest.NewRecorder()
	a.handleGmailCallback(w, r)
	if w.Code != http.StatusFound || w.Header().Get("Location") != "/admin" {
		t.Fatalf("callback %d %q: %s", w.Code, w.Header().Get("Location"), w.Body)
	}
	addr, token, _ := st.Mailbox(ctx, "nkirsch")
	if addr != "nick@example.com" || token == "" || strings.Contains(token, "refresh-1") {
		t.Fatalf("mailbox %q token %q", addr, token)
	}
	if got := a.Mailbox(signedIn(httptest.NewRequest(http.MethodGet, "/admin", nil))); got != "nick@example.com" {
		t.Errorf("Mailbox = %q", got)
	}

	from, err := a.Mail(signedIn(httptest.NewRequest(http.MethodPost, "/", nil)), "k@example.org", "You're on the board", "Hi\nthere\n")
	if err != nil || from != "nick@example.com" {
		t.Fatalf("Mail = %q, %v", from, err)
	}
	if len(sent) != 1 {
		t.Fatalf("%d sent", len(sent))
	}
	for _, want := range []string{"From: <nick@example.com>\r\n", "To: <k@example.org>\r\n",
		"Subject: You're on the board\r\n", "\r\n\r\nHi\r\nthere\r\n"} {
		if !strings.Contains(sent[0], want) {
			t.Errorf("message missing %q:\n%s", want, sent[0])
		}
	}
}

// Connecting needs a signed-in session and Google configured.
func TestGmailConnectNeedsSession(t *testing.T) {
	a, _ := New(Config{BaseURL: "https://builder.example", SessionKey: []byte("0123456789abcdef"), GoogleClientID: "g"}, nil)
	w := httptest.NewRecorder()
	a.handleGmail(w, httptest.NewRequest(http.MethodGet, "/auth/gmail", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("no mailboxes: %d", w.Code)
	}
	a.cfg.Mailboxes = memMailboxes{}
	w = httptest.NewRecorder()
	a.handleGmail(w, httptest.NewRequest(http.MethodGet, "/auth/gmail?next=/admin", nil))
	if w.Code != http.StatusFound || w.Header().Get("Location") != "/auth/login" {
		t.Fatalf("signed out: %d %q", w.Code, w.Header().Get("Location"))
	}
	r := httptest.NewRequest(http.MethodGet, "/auth/gmail?next=/admin", nil)
	r.AddCookie(a.SessionCookie("nick@example.com"))
	w = httptest.NewRecorder()
	a.handleGmail(w, r)
	loc := w.Header().Get("Location")
	for _, want := range []string{"accounts.google.com", "gmail.send", "access_type=offline", "prompt=consent", "auth%2Fgmail%2Fcallback"} {
		if !strings.Contains(loc, want) {
			t.Errorf("redirect %q missing %s", loc, want)
		}
	}
}

func TestSealRoundTrip(t *testing.T) {
	a, _ := New(Config{BaseURL: "https://builder.example", SessionKey: []byte("0123456789abcdef")}, nil)
	s, err := a.seal("refresh-1")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := a.unseal(s); err != nil || got != "refresh-1" {
		t.Fatalf("unseal = %q, %v", got, err)
	}
	b, _ := New(Config{BaseURL: "https://builder.example", SessionKey: []byte("fedcba9876543210")}, nil)
	if _, err := b.unseal(s); err == nil {
		t.Error("unsealed with another SESSION_KEY")
	}
}

type memMailboxes map[string][2]string

func (m memMailboxes) SetMailbox(_ context.Context, id, addr, tok string) error {
	m[id] = [2]string{addr, tok}
	return nil
}

func (m memMailboxes) Mailbox(_ context.Context, id string) (string, string, error) {
	return m[id][0], m[id][1], nil
}
