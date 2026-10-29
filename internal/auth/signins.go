package auth

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/nkirsch/auto-builder/internal/db"
)

const (
	// firstVisit is how soon after sign-in a multi-mode board records whether
	// the session is a member.
	firstVisit = 2 * time.Minute
	// revisit is how long a board's outcome for the same identity is recorded once.
	revisit = 10 * time.Minute
)

func (a *Auth) signIns() db.SignInLog {
	if a.cfg.SignIns != nil {
		return a.cfg.SignIns
	}
	if a.store != nil {
		return a.store
	}
	return nil
}

// record logs a sign-in attempt at the callbacks; next is where it was
// headed. Provider "" (connecting Gmail) isn't a sign-in.
func (a *Auth) record(r *http.Request, provider, identity, result, next string) {
	if provider == "" {
		return
	}
	board := ""
	if a.cfg.Multi && a.cfg.BoardOf != nil && next != "" {
		board = a.cfg.BoardOf(next)
	}
	a.log(r, db.SignIn{Provider: provider, Identity: identity, Result: result, Board: board}, 0)
}

// recordFirstVisit logs a multi-mode board's verdict on a session signed in
// moments ago: the hub's callback can't tell whether it's a member here.
func (a *Auth) recordFirstVisit(r *http.Request, identity string, err error) {
	if !a.tenant {
		return
	}
	if _, at := a.session(r); at.IsZero() || time.Since(at) > firstVisit {
		return
	}
	result := db.SignInOK
	switch {
	case errors.Is(err, db.ErrNotFound):
		result = db.SignInNotMember
	case errors.Is(err, errGoogleOnly):
		result = db.SignInGoogleOnly
	case err != nil:
		return
	}
	provider := "github"
	if strings.Contains(identity, "@") {
		provider = "google"
	}
	a.log(r, db.SignIn{Provider: provider, Identity: identity, Result: result, Board: strings.TrimPrefix(a.prefix, "/")}, revisit)
}

func (a *Auth) log(r *http.Request, s db.SignIn, within time.Duration) {
	l := a.signIns()
	if l == nil || a.dev {
		return
	}
	s.IP, s.UserAgent = clientIP(r), r.UserAgent()
	if err := l.RecordSignIn(context.WithoutCancel(r.Context()), s, within); err != nil {
		slog.Error("record sign-in", "err", err)
	}
}

// clientIP is the first X-Forwarded-For address (Cloud Run's front end sets
// it), else the peer's.
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		ip, _, _ := strings.Cut(xff, ",")
		return strings.TrimSpace(ip)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
