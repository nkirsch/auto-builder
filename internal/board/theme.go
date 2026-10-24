package board

import (
	"net/http"
	"net/url"
	"strings"
	"time"
)

// themeCookie holds the member's colour scheme: "light", "dark", or absent to
// follow the system preference.
const themeCookie = "theme"

// theme returns the request's chosen colour scheme, or "" for the system default.
func theme(r *http.Request) string {
	c, err := r.Cookie(themeCookie)
	if err != nil {
		return ""
	}
	switch c.Value {
	case "light", "dark":
		return c.Value
	}
	return ""
}

// setTheme stores the chosen colour scheme and returns to the page it came from.
func (b *Board) setTheme(w http.ResponseWriter, r *http.Request) {
	switch v := r.FormValue("theme"); v {
	case "light", "dark":
		http.SetCookie(w, &http.Cookie{
			Name: themeCookie, Value: v, Path: "/", MaxAge: int((365 * 24 * time.Hour).Seconds()),
			HttpOnly: true, Secure: strings.HasPrefix(b.BaseURL, "https://"), SameSite: http.SameSiteLaxMode,
		})
	default:
		http.Error(w, "theme must be light or dark", http.StatusBadRequest)
		return
	}
	http.Redirect(w, r, backPath(r.FormValue("back")), http.StatusSeeOther)
}

// backPath returns p if it is a local path on this site, else "/". The flash
// message is dropped so it isn't shown twice.
func backPath(p string) string {
	if !strings.HasPrefix(p, "/") || strings.HasPrefix(p, "//") || strings.ContainsAny(p, "\\\r\n") {
		return "/"
	}
	u, err := url.Parse(p)
	if err != nil || u.Scheme != "" || u.Host != "" {
		return "/"
	}
	q := u.Query()
	q.Del("m")
	u.RawQuery = q.Encode()
	return u.RequestURI()
}
