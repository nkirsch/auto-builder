package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// AppCallback is where /auth/app sends its code: the Iterate iOS app, which
// opens /auth/app in an ASWebAuthenticationSession (Safari's cookies, which
// Google allows) and redeems the code in its own web views.
const AppCallback = "iterate://auth"

const appCodeTTL = 2 * time.Minute

// handleApp hands a signed-in session to the app: a short-lived code bound
// to the app's PKCE challenge (S256), sent to AppCallback.
func (a *Auth) handleApp(w http.ResponseWriter, r *http.Request) {
	challenge := r.URL.Query().Get("challenge")
	if len(challenge) != 43 { // base64url of a SHA-256, unpadded
		http.Error(w, "challenge required", http.StatusBadRequest)
		return
	}
	login := a.login(r)
	if login == "" {
		http.Redirect(w, r, "/auth/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusFound)
		return
	}
	http.Redirect(w, r, AppCallback+"?code="+url.QueryEscape(a.appCode(login, challenge, time.Now().Add(appCodeTTL))), http.StatusFound)
}

// handleAppRedeem trades a code and its PKCE verifier for the session cookie.
func (a *Auth) handleAppRedeem(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	login, ok := a.redeemAppCode(q.Get("code"), q.Get("verifier"), time.Now())
	if !ok {
		http.Error(w, "sign-in code expired or invalid: sign in again", http.StatusUnauthorized)
		return
	}
	a.setSession(w, r, login, q.Get("next"))
}

// appCode is base64(login|expiry|challenge).hex(hmac), keyed apart from sessions.
func (a *Auth) appCode(login, challenge string, expiry time.Time) string {
	payload := base64.RawURLEncoding.EncodeToString([]byte(login + "|" + strconv.FormatInt(expiry.Unix(), 10) + "|" + challenge))
	return payload + "." + a.mac("app|"+payload)
}

func (a *Auth) redeemAppCode(code, verifier string, now time.Time) (string, bool) {
	payload, sig, ok := strings.Cut(code, ".")
	if !ok || verifier == "" || !hmac.Equal([]byte(sig), []byte(a.mac("app|"+payload))) {
		return "", false
	}
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return "", false
	}
	parts := strings.Split(string(raw), "|")
	if len(parts) != 3 || parts[0] == "" {
		return "", false
	}
	unix, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || now.Unix() > unix {
		return "", false
	}
	sum := sha256.Sum256([]byte(verifier))
	if !hmac.Equal([]byte(base64.RawURLEncoding.EncodeToString(sum[:])), []byte(parts[2])) {
		return "", false
	}
	return parts[0], true
}
