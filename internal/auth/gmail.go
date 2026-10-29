package auth

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"mime"
	"net/http"
	"net/mail"
	"strings"

	"golang.org/x/oauth2"
)

// Mailboxes keeps each identity's connected Gmail: its address and the
// sealed refresh token. Unknown identities have none ("", "", nil).
type Mailboxes interface {
	SetMailbox(ctx context.Context, identity, address, token string) error
	Mailbox(ctx context.Context, identity string) (address, token string, err error)
}

// ErrNoMailbox means the signed-in admin hasn't connected Gmail.
var ErrNoMailbox = errors.New("Gmail not connected")

func (a *Auth) mailboxes() Mailboxes {
	if a.cfg.Mailboxes != nil {
		return a.cfg.Mailboxes
	}
	if a.store != nil {
		return a.store
	}
	return nil
}

// CanMail reports whether admins can connect Gmail to send mail.
func (a *Auth) CanMail() bool { return a.Google() && a.mailboxes() != nil }

// handleGmail starts connecting the signed-in identity's Gmail.
func (a *Auth) handleGmail(w http.ResponseWriter, r *http.Request) {
	if !a.CanMail() || a.dev {
		http.Error(w, "Gmail needs Google sign-in configured (GOOGLE_CLIENT_ID)", http.StatusServiceUnavailable)
		return
	}
	if a.login(r) == "" {
		http.Redirect(w, r, "/auth/login", http.StatusFound)
		return
	}
	a.start(w, r, a.gmail, oauth2.AccessTypeOffline, oauth2.ApprovalForce)
}

// handleGmailCallback stores the refresh token for the session's identity.
func (a *Auth) handleGmailCallback(w http.ResponseWriter, r *http.Request) {
	next, ok := a.checkState(w, r, "")
	if !ok {
		return
	}
	identity := a.login(r)
	if identity == "" {
		http.Error(w, "sign in first", http.StatusUnauthorized)
		return
	}
	tok, err := a.gmail.Exchange(r.Context(), r.URL.Query().Get("code"))
	if err != nil || tok.RefreshToken == "" {
		slog.Warn("gmail exchange", "err", err)
		http.Error(w, "connecting Gmail failed", http.StatusBadGateway)
		return
	}
	user, err := a.googleUser(r.Context(), tok.AccessToken)
	if err != nil {
		slog.Warn("gmail user", "err", err)
		http.Error(w, "connecting Gmail failed", http.StatusBadGateway)
		return
	}
	sealed, err := a.seal(tok.RefreshToken)
	if err == nil {
		err = a.mailboxes().SetMailbox(r.Context(), identity, strings.ToLower(user.Email), sealed)
	}
	if err != nil {
		slog.Error("set mailbox", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, safeNext(next), http.StatusFound)
}

// Mailbox is the Gmail address the signed-in identity sends from, or "".
func (a *Auth) Mailbox(r *http.Request) string {
	m := a.mailboxes()
	if m == nil {
		return ""
	}
	addr, _, err := m.Mailbox(r.Context(), a.login(r))
	if err != nil {
		slog.Error("mailbox", "err", err)
	}
	return addr
}

// Mail sends a plain-text message from the signed-in identity's Gmail,
// returning the address it went from.
func (a *Auth) Mail(r *http.Request, to, subject, body string) (string, error) {
	m := a.mailboxes()
	if m == nil {
		return "", ErrNoMailbox
	}
	from, sealed, err := m.Mailbox(r.Context(), a.login(r))
	if err != nil {
		return "", err
	}
	if from == "" {
		return "", ErrNoMailbox
	}
	refresh, err := a.unseal(sealed)
	if err != nil {
		return "", fmt.Errorf("%w (reconnect)", ErrNoMailbox)
	}
	ctx := r.Context()
	client := a.gmail.Client(ctx, &oauth2.Token{RefreshToken: refresh})
	raw := base64.RawURLEncoding.EncodeToString(message(from, to, subject, body))
	payload, _ := json.Marshal(map[string]string{"raw": raw})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, a.gmailAPI+"/gmail/v1/users/me/messages/send", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return from, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return from, fmt.Errorf("gmail send: %s", resp.Status)
	}
	return from, nil
}

// message is an RFC 5322 plain-text message.
func message(from, to, subject, body string) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "From: %s\r\nTo: %s\r\nSubject: %s\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=UTF-8\r\n\r\n",
		(&mail.Address{Address: from}).String(), (&mail.Address{Address: to}).String(), mime.QEncoding.Encode("UTF-8", subject))
	b.WriteString(strings.ReplaceAll(body, "\n", "\r\n"))
	return b.Bytes()
}

// seal encrypts a refresh token with a key derived from SESSION_KEY.
func (a *Auth) seal(plain string) (string, error) {
	gcm, err := a.aead()
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	rand.Read(nonce)
	return base64.RawURLEncoding.EncodeToString(gcm.Seal(nonce, nonce, []byte(plain), nil)), nil
}

func (a *Auth) unseal(sealed string) (string, error) {
	gcm, err := a.aead()
	if err != nil {
		return "", err
	}
	raw, err := base64.RawURLEncoding.DecodeString(sealed)
	if err != nil || len(raw) < gcm.NonceSize() {
		return "", errors.New("bad sealed token")
	}
	plain, err := gcm.Open(nil, raw[:gcm.NonceSize()], raw[gcm.NonceSize():], nil)
	return string(plain), err
}

func (a *Auth) aead() (cipher.AEAD, error) {
	key := sha256.Sum256(append([]byte("gmail-token\x00"), a.cfg.SessionKey...))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
