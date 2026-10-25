// Package board serves the item board: items, threads, attachments, inbox,
// run history and admin.
package board

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/nkirsch/auto-builder/internal/auth"
	"github.com/nkirsch/auto-builder/internal/db"
	"github.com/nkirsch/auto-builder/internal/notify"
	"github.com/nkirsch/auto-builder/internal/store"
	"github.com/nkirsch/auto-builder/web"
)

const (
	maxAttachment = 10 << 20
	maxUpload     = 25 << 20
)

// Gate is what the board asks of the run gate.
type Gate interface {
	// Activity re-evaluates the gate after a human change; it must not block.
	Activity()
	RunNow(ctx context.Context, by string) (int, error)
}

// Board holds the board's dependencies.
type Board struct {
	Store    *db.Store
	Storage  store.Storage
	Auth     *auth.Auth
	Pushover *notify.Pushover
	Gate     Gate
	BaseURL  string
	BotLogin string
	GitSHA   string
	Repo     string

	tmpl    map[string]*template.Template
	version atomic.Pointer[string]
}

// SetVersion sets the release name shown next to the brand, e.g. "v26.10.25-2".
func (b *Board) SetVersion(v string) { b.version.Store(&v) }

// Version is the release name set by SetVersion, or "" until then.
func (b *Board) Version() string {
	if v := b.version.Load(); v != nil {
		return *v
	}
	return ""
}

// Routes registers the board's pages.
func (b *Board) Routes(mux *http.ServeMux) error {
	if err := b.parse(); err != nil {
		return err
	}
	member := func(h http.HandlerFunc) http.Handler { return b.Auth.Require(h) }
	admin := func(h http.HandlerFunc) http.Handler { return b.Auth.RequireAdmin(h) }

	mux.Handle("GET /static/", http.FileServerFS(web.FS))
	mux.HandleFunc("GET /auth/login", b.login)
	mux.HandleFunc("GET /favicon.ico", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/static/icon.svg", http.StatusMovedPermanently)
	})
	mux.Handle("GET /{$}", member(b.index))
	mux.Handle("GET /items/new", member(b.newItem))
	mux.Handle("POST /items", member(b.createItem))
	mux.Handle("GET /items/{id}", member(b.item))
	mux.Handle("POST /items/{id}/responses", member(b.reply))
	mux.Handle("POST /items/{id}/close", member(b.closeItem))
	mux.Handle("POST /items/{id}/reopen", member(b.reopenItem))
	mux.Handle("POST /items/{id}/approve", admin(b.approve))
	mux.Handle("GET /attachments/{id}", member(b.attachment))
	mux.Handle("GET /inbox", member(b.inbox))
	mux.Handle("POST /inbox/read", member(b.markAllRead))
	mux.Handle("GET /runs", member(b.runs))
	mux.Handle("GET /runs/{id}/log", member(b.runLog))
	mux.Handle("GET /admin", admin(b.admin))
	mux.Handle("POST /admin/members", admin(b.addMember))
	mux.Handle("POST /admin/members/{login}/delete", admin(b.deleteMember))
	mux.Handle("POST /admin/run", admin(b.runNow))
	mux.Handle("POST /admin/spend-limit", admin(b.setSpendLimit))
	mux.Handle("POST /theme", member(b.setTheme))
	return nil
}

var funcs = template.FuncMap{
	"md":  Markdown,
	"usd": func(v float64) string { return fmt.Sprintf("$%.2f", v) },
	"ago": func(t time.Time) string {
		d := time.Since(t)
		switch {
		case d < time.Minute:
			return "just now"
		case d < time.Hour:
			return fmt.Sprintf("%dm ago", int(d.Minutes()))
		case d < 48*time.Hour:
			return fmt.Sprintf("%dh ago", int(d.Hours()))
		default:
			return t.Format("Jan 2")
		}
	},
	"dur":   dur,
	"stamp": func(t time.Time) string { return t.UTC().Format(time.RFC3339) },
	"short": func(s string) string {
		if len(s) > 7 {
			return s[:7]
		}
		return s
	},
	"size": func(n int64) string {
		switch {
		case n >= 1<<20:
			return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
		case n >= 1<<10:
			return fmt.Sprintf("%d KB", n>>10)
		default:
			return fmt.Sprintf("%d B", n)
		}
	},
	"deref": func(p *int) int {
		if p == nil {
			return 0
		}
		return *p
	},
}

func (b *Board) parse() error {
	b.tmpl = map[string]*template.Template{}
	pages, err := web.Pages()
	if err != nil {
		return err
	}
	for _, p := range pages {
		t, err := template.New("").Funcs(funcs).ParseFS(web.FS, "templates/layout.html", "templates/"+p)
		if err != nil {
			return fmt.Errorf("template %s: %w", p, err)
		}
		b.tmpl[strings.TrimSuffix(p, ".html")] = t
	}
	return nil
}

// page is the data every template gets.
type page struct {
	Me      db.Member
	Unread  int
	Title   string
	GitSHA  string
	Repo    string // owner/name on GitHub, for the footer's commit link
	Version string // "vYY.MM.DD-N", or "" until known
	Flash   string
	BotName string
	Theme   string // "light", "dark" or "" (system)
	Path    string // current page, for forms that return here
	BaseURL string // absolute links for link previews
	Data    any
}

func (b *Board) render(w http.ResponseWriter, r *http.Request, name, title string, data any) {
	me := auth.Member(r.Context())
	var unread int
	if me.Login != "" {
		var err error
		if unread, err = b.Store.UnreadCount(r.Context(), me.Login); err != nil {
			slog.Warn("unread count", "err", err)
		}
	}
	t, ok := b.tmpl[name]
	if !ok {
		http.Error(w, "no template "+name, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	p := page{Me: me, Unread: unread, Title: title, GitSHA: b.GitSHA, Repo: b.Repo, Flash: r.URL.Query().Get("m"),
		BotName: b.BotLogin, Theme: theme(r), Path: r.URL.RequestURI(), BaseURL: strings.TrimSuffix(b.BaseURL, "/"), Data: data}
	p.Version = b.Version()
	if err := t.ExecuteTemplate(w, "layout", p); err != nil {
		slog.Error("render", "page", name, "err", err)
	}
}

func (b *Board) fail(w http.ResponseWriter, err error) {
	if errors.Is(err, db.ErrNotFound) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	slog.Error("board", "err", err)
	http.Error(w, "internal error", http.StatusInternalServerError)
}

func pathID(r *http.Request) (int, bool) {
	id, err := strconv.Atoi(r.PathValue("id"))
	return id, err == nil && id > 0
}

// redirect sends a POST-redirect-GET with an optional flash message.
func redirect(w http.ResponseWriter, r *http.Request, to, msg string) {
	if msg != "" {
		// The flash goes in the query, which must precede any #fragment.
		path, frag, hasFrag := strings.Cut(to, "#")
		sep := "?"
		if strings.Contains(path, "?") {
			sep = "&"
		}
		to = path + sep + "m=" + urlQuery(msg)
		if hasFrag {
			to += "#" + frag
		}
	}
	http.Redirect(w, r, to, http.StatusSeeOther)
}

func urlQuery(s string) string { return strings.ReplaceAll(template.URLQueryEscaper(s), "+", "%20") }

// Pages

func (b *Board) index(w http.ResponseWriter, r *http.Request) {
	show := r.URL.Query().Get("show")
	statuses := []string{"open", "building", "merged"}
	if show == "done" {
		statuses = []string{"shipped", "closed"}
	}
	items, err := b.Store.Items(r.Context(), statuses...)
	if err != nil {
		b.fail(w, err)
		return
	}
	tag := strings.ToLower(r.URL.Query().Get("tag"))
	if tag != "" {
		items = slices.DeleteFunc(items, func(it db.Item) bool { return !slices.Contains(it.Tags, tag) })
	}
	b.render(w, r, "index", "Board", map[string]any{"Sections": sections(items, show == "done"), "Show": show, "Tag": tag})
}

// section is a titled group of items on the board.
type section struct {
	Title string
	Items []db.Item
}

// sections groups the Open view into waiting for approval, approved but not
// started, and in progress (a PR is open or merged); empty groups are left
// out. The Done view is one untitled list.
func sections(items []db.Item, done bool) []section {
	if done {
		if len(items) == 0 {
			return nil
		}
		return []section{{Items: items}}
	}
	out := []section{{Title: "Waiting for approval"}, {Title: "Approved, not started"}, {Title: "In progress"}}
	for _, it := range items {
		i := 2
		if it.Status == "open" {
			i = 0
			if it.Approved {
				i = 1
			}
		}
		out[i].Items = append(out[i].Items, it)
	}
	return slices.DeleteFunc(out, func(s section) bool { return len(s.Items) == 0 })
}

func (b *Board) newItem(w http.ResponseWriter, r *http.Request) {
	b.render(w, r, "new", "New ask", nil)
}

func (b *Board) createItem(w http.ResponseWriter, r *http.Request) {
	me := auth.Member(r.Context())
	if err := parseForm(w, r); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	title := strings.TrimSpace(r.FormValue("title"))
	body := strings.TrimSpace(r.FormValue("body"))
	if title == "" {
		http.Error(w, "title is required", http.StatusBadRequest)
		return
	}
	if len(title) > 200 || len(body) > 50_000 {
		http.Error(w, "title or body too long", http.StatusBadRequest)
		return
	}
	id, err := b.Store.CreateItem(r.Context(), title, body, me.Login)
	if err != nil {
		b.fail(w, err)
		return
	}
	if tags := Tags(title + "\n" + body); len(tags) > 0 {
		if err := b.Store.SetTags(r.Context(), id, tags, false); err != nil {
			slog.Warn("tags", "item", id, "err", err)
		}
	}
	if err := b.saveUploads(r, id, me.Login); err != nil {
		slog.Warn("upload", "item", id, "err", err)
	}
	b.mention(r.Context(), id, me.Login, body)
	if !me.IsAdmin() {
		b.Pushover.Send(context.WithoutCancel(r.Context()), "New ask from "+me.Login, title, b.itemURL(id))
	}
	b.Gate.Activity()
	redirect(w, r, fmt.Sprintf("/items/%d", id), "")
}

func (b *Board) item(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	ctx := r.Context()
	it, err := b.Store.Item(ctx, id)
	if err != nil {
		b.fail(w, err)
		return
	}
	responses, err := b.Store.Responses(ctx, id)
	if err != nil {
		b.fail(w, err)
		return
	}
	atts, err := b.Store.Attachments(ctx, id)
	if err != nil {
		b.fail(w, err)
		return
	}
	me := auth.Member(ctx)
	if err := b.Store.MarkRead(ctx, me.Login, id); err != nil {
		slog.Warn("mark read", "err", err)
	}
	b.render(w, r, "item", it.Title, map[string]any{
		"Item": it, "Responses": responses, "Attachments": atts,
		"CanClose": me.IsAdmin() || strings.EqualFold(me.Login, it.Author),
		"Repo":     b.Repo,
	})
}

func (b *Board) reply(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	me := auth.Member(r.Context())
	if err := parseForm(w, r); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	body := strings.TrimSpace(r.FormValue("body"))
	hasFiles := r.MultipartForm != nil && len(r.MultipartForm.File["files"]) > 0
	if body == "" && !hasFiles {
		redirect(w, r, fmt.Sprintf("/items/%d", id), "")
		return
	}
	if len(body) > 50_000 {
		http.Error(w, "reply too long", http.StatusBadRequest)
		return
	}
	it, err := b.Store.Item(r.Context(), id)
	if err != nil {
		b.fail(w, err)
		return
	}
	// An admin replying on a finished item wants more work done on it.
	reopened := false
	if me.IsAdmin() && (it.Status == "closed" || it.Status == "shipped") {
		if err := b.Store.ReopenItem(r.Context(), id); err != nil {
			b.fail(w, err)
			return
		}
		reopened = true
	}
	if body != "" {
		if _, err := b.Store.AddResponse(r.Context(), id, me.Login, body, true); err != nil {
			b.fail(w, err)
			return
		}
	}
	if err := b.saveUploads(r, id, me.Login); err != nil {
		slog.Warn("upload", "item", id, "err", err)
	}
	b.Notified(r.Context(), it, me.Login, body)
	if !me.IsAdmin() {
		b.Pushover.Send(context.WithoutCancel(r.Context()), me.Login+" replied: "+it.Title, body, b.itemURL(id))
	}
	b.Gate.Activity()
	msg := ""
	if reopened {
		msg = "Reopened"
	}
	redirect(w, r, fmt.Sprintf("/items/%d#end", id), msg)
}

// Notified creates in-app notifications for a reply: the item's author and
// anyone @-mentioned, never the actor.
func (b *Board) Notified(ctx context.Context, it db.Item, actor, body string) {
	notified := map[string]bool{strings.ToLower(actor): true}
	for _, login := range Mentions(body) {
		if notified[login] {
			continue
		}
		notified[login] = true
		if b.isMember(ctx, login) {
			b.notify(ctx, login, it.ID, actor, "mention")
		}
	}
	if author := strings.ToLower(it.Author); !notified[author] && b.isMember(ctx, author) {
		b.notify(ctx, it.Author, it.ID, actor, "reply")
	}
}

func (b *Board) mention(ctx context.Context, itemID int, actor, body string) {
	for _, login := range Mentions(body) {
		if !strings.EqualFold(login, actor) && b.isMember(ctx, login) {
			b.notify(ctx, login, itemID, actor, "mention")
		}
	}
}

func (b *Board) isMember(ctx context.Context, login string) bool {
	m, err := b.Store.Member(ctx, login)
	return err == nil && m.Login != ""
}

func (b *Board) notify(ctx context.Context, login string, itemID int, actor, kind string) {
	if m, err := b.Store.Member(ctx, login); err == nil {
		login = m.Login
	}
	if err := b.Store.Notify(ctx, login, itemID, actor, kind); err != nil {
		slog.Warn("notify", "login", login, "err", err)
	}
}

func (b *Board) closeItem(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	me := auth.Member(r.Context())
	it, err := b.Store.Item(r.Context(), id)
	if err != nil {
		b.fail(w, err)
		return
	}
	if !me.IsAdmin() && !strings.EqualFold(me.Login, it.Author) {
		http.Error(w, "only the author or an admin can close this", http.StatusForbidden)
		return
	}
	note := strings.TrimSpace(r.FormValue("note"))
	if note == "" {
		note = "Closed by " + me.Login
	}
	if err := b.Store.CloseItem(r.Context(), id, note, true); err != nil {
		b.fail(w, err)
		return
	}
	redirect(w, r, fmt.Sprintf("/items/%d", id), "Closed")
}

func (b *Board) reopenItem(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	me := auth.Member(r.Context())
	it, err := b.Store.Item(r.Context(), id)
	if err != nil {
		b.fail(w, err)
		return
	}
	if !me.IsAdmin() && !strings.EqualFold(me.Login, it.Author) {
		http.Error(w, "only the author or an admin can reopen this", http.StatusForbidden)
		return
	}
	if err := b.Store.ReopenItem(r.Context(), id); err != nil {
		b.fail(w, err)
		return
	}
	b.Gate.Activity()
	redirect(w, r, fmt.Sprintf("/items/%d", id), "Reopened")
}

func (b *Board) approve(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	approved := r.FormValue("approved") != "false"
	if err := b.Store.SetApproved(r.Context(), id, approved, auth.Member(r.Context()).Login); err != nil {
		b.fail(w, err)
		return
	}
	msg := "Unapproved"
	if approved {
		msg = "Approved"
		b.Gate.Activity()
	}
	redirect(w, r, fmt.Sprintf("/items/%d", id), msg)
}

// Attachments

func parseForm(w http.ResponseWriter, r *http.Request) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxUpload+1<<20)
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		if err := r.ParseMultipartForm(8 << 20); err != nil {
			return fmt.Errorf("upload too large (max %d MB)", maxUpload>>20)
		}
		return nil
	}
	return r.ParseForm()
}

var unsafeName = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

func (b *Board) saveUploads(r *http.Request, itemID int, by string) error {
	if r.MultipartForm == nil {
		return nil
	}
	var errs []error
	for _, fh := range r.MultipartForm.File["files"] {
		if fh.Size == 0 {
			continue
		}
		if fh.Size > maxAttachment {
			errs = append(errs, fmt.Errorf("%s: over %d MB", fh.Filename, maxAttachment>>20))
			continue
		}
		f, err := fh.Open()
		if err != nil {
			errs = append(errs, err)
			continue
		}
		name := path.Base(strings.ReplaceAll(fh.Filename, "\\", "/"))
		ctype := fh.Header.Get("Content-Type")
		if _, _, err := mime.ParseMediaType(ctype); err != nil || ctype == "" {
			ctype = "application/octet-stream"
		}
		object := fmt.Sprintf("attachments/%d/%s-%s", itemID, randHex(6), unsafeName.ReplaceAllString(name, "_"))
		err = b.Storage.Put(r.Context(), object, ctype, f)
		f.Close()
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if _, err := b.Store.AddAttachment(r.Context(), db.Attachment{
			ItemID: itemID, Name: name, ContentType: ctype, Size: fh.Size, Object: object, UploadedBy: by,
		}); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func randHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// inlineTypes are shown in the browser; everything else downloads.
var inlineTypes = map[string]bool{"image/png": true, "image/jpeg": true, "image/gif": true, "image/webp": true}

func (b *Board) attachment(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	a, err := b.Store.Attachment(r.Context(), id)
	if err != nil {
		b.fail(w, err)
		return
	}
	b.serveObject(w, r, a.Object, a.Name, a.ContentType)
}

func (b *Board) serveObject(w http.ResponseWriter, r *http.Request, object, name, ctype string) {
	rc, err := b.Storage.Get(r.Context(), object)
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		b.fail(w, err)
		return
	}
	defer rc.Close()
	disposition := "attachment"
	if inlineTypes[ctype] || ctype == "text/plain; charset=utf-8" {
		disposition = "inline"
	} else {
		ctype = "application/octet-stream"
	}
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Content-Disposition", mime.FormatMediaType(disposition, map[string]string{"filename": name}))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "sandbox; default-src 'none'; img-src 'self'")
	w.Header().Set("Cache-Control", "private, max-age=3600")
	io.Copy(w, rc)
}

// Inbox

func (b *Board) inbox(w http.ResponseWriter, r *http.Request) {
	me := auth.Member(r.Context())
	show := r.URL.Query().Get("show")
	ns, err := b.Store.Notifications(r.Context(), me.Login, show == "done", 100)
	if err != nil {
		b.fail(w, err)
		return
	}
	b.render(w, r, "inbox", "Inbox", map[string]any{"Notifications": ns, "Show": show})
}

func (b *Board) markAllRead(w http.ResponseWriter, r *http.Request) {
	if err := b.Store.MarkRead(r.Context(), auth.Member(r.Context()).Login, 0); err != nil {
		b.fail(w, err)
		return
	}
	redirect(w, r, "/inbox", "")
}

// Runs

// dur formats a duration compactly: "45s", "3m 05s", "1h 02m".
func dur(d time.Duration) string {
	d = d.Round(time.Second)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm %02ds", int(d.Minutes()), int(d.Seconds())%60)
	default:
		return fmt.Sprintf("%dh %02dm", int(d.Hours()), int(d.Minutes())%60)
	}
}

// runRow is a run with the items it acted on.
type runRow struct {
	db.Run
	Items []db.RunItem
}

func (b *Board) runs(w http.ResponseWriter, r *http.Request) {
	runs, err := b.Store.Runs(r.Context(), 50)
	if err != nil {
		b.fail(w, err)
		return
	}
	ids := make([]int, len(runs))
	for i, run := range runs {
		ids[i] = run.ID
	}
	items, err := b.Store.RunItems(r.Context(), ids)
	if err != nil {
		b.fail(w, err)
		return
	}
	rows := make([]runRow, len(runs))
	for i, run := range runs {
		rows[i] = runRow{Run: run, Items: items[run.ID]}
	}
	b.render(w, r, "runs", "Runs", rows)
}

func (b *Board) runLog(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	run, err := b.Store.Run(r.Context(), id)
	if err != nil {
		b.fail(w, err)
		return
	}
	if run.LogObject == "" {
		http.Error(w, "no log for this run", http.StatusNotFound)
		return
	}
	b.serveObject(w, r, run.LogObject, fmt.Sprintf("run-%d.log", id), "text/plain; charset=utf-8")
}

// Admin

func (b *Board) admin(w http.ResponseWriter, r *http.Request) {
	members, err := b.Store.Members(r.Context())
	if err != nil {
		b.fail(w, err)
		return
	}
	costs, err := b.Store.RunCosts(r.Context())
	if err != nil {
		b.fail(w, err)
		return
	}
	limit, err := b.Store.SpendLimit(r.Context())
	if err != nil {
		b.fail(w, err)
		return
	}
	byMember, err := b.Store.MemberCosts(r.Context())
	if err != nil {
		b.fail(w, err)
		return
	}
	recent, err := b.Store.RunStats(r.Context(), time.Now().Add(-30*24*time.Hour))
	if err != nil {
		b.fail(w, err)
		return
	}
	all, err := b.Store.RunStats(r.Context(), time.Time{})
	if err != nil {
		b.fail(w, err)
		return
	}
	b.render(w, r, "admin", "Admin", map[string]any{"Members": members, "Costs": costs,
		"Limit": limit, "MemberCosts": byMember, "Stats": []db.RunStats{recent, all}})
}

func (b *Board) setSpendLimit(w http.ResponseWriter, r *http.Request) {
	v, err := strconv.ParseFloat(strings.TrimPrefix(strings.TrimSpace(r.FormValue("limit")), "$"), 64)
	if err != nil || !(v >= 0.01 && v < 1e6) {
		redirect(w, r, "/admin", "Enter a spend limit in dollars, at least $0.01")
		return
	}
	if err := b.Store.SetSpendLimit(r.Context(), v); err != nil {
		b.fail(w, err)
		return
	}
	b.Gate.Activity() // raising the limit may release held work
	redirect(w, r, "/admin", fmt.Sprintf("Spend limit set to $%.2f", v))
}

var loginRE = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9]|-[A-Za-z0-9]){0,38}$`)

func (b *Board) addMember(w http.ResponseWriter, r *http.Request) {
	login := strings.TrimPrefix(strings.TrimSpace(r.FormValue("login")), "@")
	role := r.FormValue("role")
	if !loginRE.MatchString(login) || (role != "admin" && role != "member") {
		redirect(w, r, "/admin", "Enter a GitHub login and a role")
		return
	}
	if strings.EqualFold(login, b.BotLogin) {
		redirect(w, r, "/admin", "That's the bot's login")
		return
	}
	if err := b.Store.UpsertMember(r.Context(), login, role, auth.Member(r.Context()).Login); err != nil {
		b.fail(w, err)
		return
	}
	redirect(w, r, "/admin", "Saved "+login)
}

func (b *Board) deleteMember(w http.ResponseWriter, r *http.Request) {
	login := r.PathValue("login")
	if strings.EqualFold(login, auth.Member(r.Context()).Login) {
		redirect(w, r, "/admin", "You can't remove yourself")
		return
	}
	if err := b.Store.DeleteMember(r.Context(), login); err != nil {
		b.fail(w, err)
		return
	}
	redirect(w, r, "/admin", "Removed "+login)
}

func (b *Board) runNow(w http.ResponseWriter, r *http.Request) {
	id, err := b.Gate.RunNow(r.Context(), auth.Member(r.Context()).Login)
	switch {
	case errors.Is(err, db.ErrRunActive):
		redirect(w, r, "/runs", "A run is already active")
	case errors.Is(err, db.ErrSpendLimit):
		redirect(w, r, "/admin", "Spend limit reached; raise it to run")
	case err != nil:
		slog.Error("run now", "err", err)
		redirect(w, r, "/runs", "Launch failed: "+err.Error())
	default:
		redirect(w, r, "/runs", fmt.Sprintf("Started run %d", id))
	}
}

func (b *Board) itemURL(id int) string {
	return fmt.Sprintf("%s/items/%d", strings.TrimSuffix(b.BaseURL, "/"), id)
}

// login is the signed-out landing page; its link preview is generic on purpose
// so item titles never reach Slack or iMessage.
func (b *Board) login(w http.ResponseWriter, r *http.Request) {
	next := auth.SafeNext(r.URL.Query().Get("next"))
	if b.Auth.SignedIn(r) != "" {
		http.Redirect(w, r, next, http.StatusFound)
		return
	}
	b.render(w, r, "login", "Sign in", map[string]string{"Next": next})
}
