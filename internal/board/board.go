// Package board serves the item board: items, threads, attachments, unread
// markers, run history and admin.
package board

import (
	"cmp"
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
	"net/url"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/nkirsch/auto-builder/internal/auth"
	"github.com/nkirsch/auto-builder/internal/db"
	"github.com/nkirsch/auto-builder/internal/github"
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
	// Cancel stops an active run; a finished one is db.ErrNotFound.
	Cancel(ctx context.Context, id int, by string) error
}

// Filer files a new item, opening its GitHub issue first.
type Filer interface {
	File(ctx context.Context, title, body, author string, bot, private bool) (int, error)
}

// Welcomer @-mentions a new member on GitHub, returning the issue it used.
type Welcomer interface {
	Welcome(ctx context.Context, name, login string) (int, error)
}

// Mailer sends mail from the signed-in admin's connected Gmail.
type Mailer interface {
	// Mailbox is the address the request's admin sends from, "" if unconnected.
	Mailbox(r *http.Request) string
	Mail(r *http.Request, to, subject, body string) (from string, err error)
}

// Board holds the board's dependencies.
type Board struct {
	Store    *db.Store
	Storage  store.Storage
	Auth     *auth.Auth
	Pushover *notify.Pushover
	// Quiet is the login whose own actions don't Pushover (the person receiving them).
	Quiet string
	// GitHub and Releases enable the runs page's Release button.
	GitHub   *github.Client
	Releases bool
	// Ready, when set (a hub's tenant board), reports whether the app is
	// provisioned; until it is, the runs page skips the GitHub lookups.
	Ready func(ctx context.Context) bool
	// InviteHold, when set (a hub's tenant board), says why invites must wait
	// (the app isn't ready: setting up, or its domain and certificate pending),
	// or "" once they needn't.
	InviteHold func(ctx context.Context) string
	// CreateAppURL, when set, is a hub's New app page, linked in the nav for members.
	CreateAppURL string
	Gate         Gate
	// Filer, when set (issue sync), files items under their GitHub issue's number.
	Filer Filer
	// Welcomer, when set (issue sync), greets members without an email on GitHub;
	// the rest get an email invite instead of a public @-mention.
	Welcomer Welcomer
	BaseURL  string
	BotLogin string
	GitSHA   string
	// Deployed, when set (a hub's tenant board), reports the app's live SHA;
	// GitSHA and the version are then the hub's.
	Deployed func(ctx context.Context) (string, error)
	Repo     string
	// CodeRepo, when set (a hub's tenant board), is the repo GitSHA and the
	// version belong to, for the footer; Repo is the one the board builds.
	CodeRepo string
	// Name and SiteURL, when set (a multi-tenant app's board), replace the
	// brand's name and add a Site link.
	Name    string
	SiteURL string
	// Upstream, when set, is the repo this one forks; admins can keep up with it.
	Upstream string
	// Traffic, when set, links the admin page to the site's traffic at /logs.
	Traffic bool
	// HubLimits: the hub's platform admins set the spend limit; board admins only see it.
	HubLimits bool
	// Platform, when set (multi-tenant mode), is whom admins escalate to.
	Platform Platform
	// Mailer, when set, lets admins email invites instead of opening a mail client.
	Mailer Mailer
	// Provisioning, when set (a hub's tenant board), returns the app's setup
	// progress until it's ready, then nil.
	Provisioning func(ctx context.Context) *Provisioning
	// Reprovision, when set, re-runs the app's failed setup.
	Reprovision func(ctx context.Context) error
	// SignIns, when set (a hub's tenant board), keeps sign-in attempts at
	// every board, this one's under Slug; else the board's store has them.
	SignIns db.SignInLog
	Slug    string

	tmpl    map[string]*template.Template
	version atomic.Pointer[string]
	history historyCache
	pending pendingCache
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
		w.Header().Set("Cache-Control", "public, max-age=86400")
		http.ServeFileFS(w, r, web.FS, "static/favicon.ico")
	})
	mux.Handle("GET /{$}", member(b.index))
	mux.Handle("GET /items/new", member(b.newItem))
	mux.Handle("POST /items", member(b.createItem))
	mux.Handle("GET /items/{id}", member(b.item))
	mux.Handle("POST /items/{id}/responses", member(b.reply))
	mux.Handle("POST /items/{id}/close", member(b.closeItem))
	mux.Handle("POST /items/{id}/reopen", member(b.reopenItem))
	mux.Handle("POST /items/{id}/approve", admin(b.approve))
	mux.Handle("POST /items/{id}/unblock", admin(b.unblock))
	mux.Handle("POST /items/{id}/escalate", member(b.escalate))
	mux.Handle("GET /attachments/{id}", member(b.attachment))
	mux.HandleFunc("GET /inbox", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/", http.StatusMovedPermanently)
	})
	mux.Handle("POST /read", member(b.markAllRead))
	mux.Handle("GET /runs", member(b.runs))
	mux.Handle("GET /history", member(b.historyPage))
	mux.Handle("GET /runs/{id}/log", member(b.runLog))
	mux.Handle("GET /admin", admin(b.admin))
	mux.Handle("POST /admin/members", admin(b.addMember))
	mux.Handle("POST /admin/members/{login}/delete", admin(b.deleteMember))
	mux.Handle("POST /admin/members/{login}/github", admin(b.setMemberGitHub))
	mux.Handle("POST /admin/members/{login}/email", admin(b.setMemberEmail))
	mux.Handle("GET /admin/signins", admin(b.signIns))
	mux.Handle("POST /admin/members/{login}/invite", admin(b.sendInvite))
	mux.Handle("POST /admin/run", admin(b.runNow))
	mux.Handle("POST /admin/provision", admin(b.reprovision))
	mux.Handle("POST /runs/{id}/cancel", admin(b.cancelRun))
	mux.Handle("POST /admin/release", admin(b.release))
	mux.Handle("POST /admin/spend-limit", admin(b.setSpendLimit))
	mux.Handle("POST /admin/auto-approve", admin(b.setAutoApprove))
	mux.Handle("POST /admin/upstream", admin(b.setUpstreamSync))
	mux.Handle("POST /admin/workflows", admin(b.setAgentWorkflows))
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
	"who": who,
	"pct": pct,
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
		t, err := template.New("").Funcs(funcs).Funcs(template.FuncMap{"base": b.prefix, "invite": b.invite}).ParseFS(web.FS, "templates/layout.html", "templates/brand.html", "templates/"+p)
		if err != nil {
			return fmt.Errorf("template %s: %w", p, err)
		}
		b.tmpl[strings.TrimSuffix(p, ".html")] = t
	}
	return nil
}

// who shows a login as its member's first name, with the full name and login
// on hover; a login without a name shows as itself.
func who(names map[string]string, login string) template.HTML {
	name := names[login]
	first, _, _ := strings.Cut(strings.TrimSpace(name), " ")
	if first == "" {
		return template.HTML(template.HTMLEscapeString(login))
	}
	return template.HTML(`<span title="` + template.HTMLEscapeString(name+" ("+login+")") + `">` + template.HTMLEscapeString(first) + `</span>`)
}

// pct is spent as a whole percent of limit, capped at 100.
func pct(spent, limit float64) int {
	if limit <= 0 {
		return 100
	}
	return min(100, int(spent/limit*100))
}

// names maps each member's login to their name, for who.
func (b *Board) names(ctx context.Context) map[string]string {
	ms, err := b.Store.Members(ctx)
	if err != nil {
		slog.Warn("member names", "err", err)
		return nil
	}
	names := make(map[string]string, len(ms))
	for _, m := range ms {
		names[m.Login] = m.Name
	}
	return names
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
	Theme   string            // "light", "dark" or "" (system)
	Path    string            // current page, for forms that return here
	BaseURL string            // absolute links for link previews
	AppName string            // a multi-tenant app's name, shown instead of the brand's
	SiteURL string            // the app's site, linked from the nav
	NewApp  string            // a hub's New app page, for members to make their own app
	Refresh int               // seconds until the page reloads itself; 0 never
	Pending int               // commits awaiting a release, for an admin's History badge; 0 none
	Names   map[string]string // member logins to names, for who
	Data    any
}

func (b *Board) render(w http.ResponseWriter, r *http.Request, name, title string, data any) {
	b.renderRefresh(w, r, name, title, data, 0)
}

// renderRefresh renders a page that reloads itself every refresh seconds.
func (b *Board) renderRefresh(w http.ResponseWriter, r *http.Request, name, title string, data any, refresh int) {
	me := auth.Member(r.Context())
	var unread int
	var names map[string]string
	if me.Login != "" {
		var err error
		if unread, err = b.Store.UnreadCount(r.Context(), me.Login); err != nil {
			slog.Warn("unread count", "err", err)
		}
		names = b.names(r.Context())
	}
	t, ok := b.tmpl[name]
	if !ok {
		http.Error(w, "no template "+name, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	p := page{Me: me, Unread: unread, Title: title, GitSHA: b.GitSHA, Repo: cmp.Or(b.CodeRepo, b.Repo), Flash: r.URL.Query().Get("m"),
		BotName: b.BotLogin, Theme: theme(r), Path: r.URL.RequestURI(), BaseURL: strings.TrimSuffix(b.BaseURL, "/"),
		AppName: b.Name, SiteURL: b.SiteURL, NewApp: b.CreateAppURL, Refresh: refresh, Names: names, Data: data}
	p.Version = b.Version()
	if me.IsAdmin() {
		p.Pending = b.pendingRelease(r.Context())
	}
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
	items, err := b.Store.Items(r.Context())
	if err != nil {
		b.fail(w, err)
		return
	}
	items = slices.DeleteFunc(items, func(it db.Item) bool { return isDone(it) != (show == "done") })
	tag := strings.ToLower(r.URL.Query().Get("tag"))
	if tag != "" {
		items = slices.DeleteFunc(items, func(it db.Item) bool { return !slices.Contains(it.Tags, tag) })
	}
	unread, err := b.Store.UnreadItems(r.Context(), auth.Member(r.Context()).Login)
	if err != nil {
		slog.Warn("unread items", "err", err)
	}
	// While the app is being set up, its progress reloads itself.
	prov, refresh := b.provisioning(r.Context()), 0
	if prov != nil && prov.Status != "failed" {
		refresh = 15
	}
	b.renderRefresh(w, r, "index", "Board", map[string]any{"Sections": sections(items, show == "done"), "Show": show, "Tag": tag,
		"Unread": unread, "Provisioning": prov}, refresh)
}

// isDone reports whether an item belongs in the Done view: shipped or closed
// and not blocked. Blocked items stay on the Open view whatever their status,
// since they still need someone to act.
func isDone(it db.Item) bool {
	return (it.Status == "shipped" || it.Status == "closed") && it.Blocked == ""
}

// section is a titled group of items on the board.
type section struct {
	Title string
	Items []db.Item
}

// sections groups the Open view into escalated, blocked (any status), waiting for
// approval, approved but not started, and in progress (a PR is open or
// merged); empty groups are left out. The Done view is one untitled list.
func sections(items []db.Item, done bool) []section {
	if done {
		if len(items) == 0 {
			return nil
		}
		return []section{{Items: items}}
	}
	out := []section{{Title: "Escalated"}, {Title: "Blocked"}, {Title: "Waiting for approval"}, {Title: "Approved, not started"}, {Title: "In progress"}}
	for _, it := range items {
		i := 4
		switch {
		case it.Escalation != "":
			i = 0
		case it.Blocked != "":
			i = 1
		case it.Status == "open" && it.Approved:
			i = 3
		case it.Status == "open":
			i = 2
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
	tags := Tags(title + "\n" + body)
	var id int
	var err error
	if b.Filer != nil {
		id, err = b.Filer.File(r.Context(), title, body, me.Login, false, slices.Contains(tags, "private"))
	} else {
		id, err = b.Store.CreateItem(r.Context(), title, body, me.Login)
	}
	if err != nil {
		b.fail(w, err)
		return
	}
	if me.IsAdmin() && me.AutoApprove {
		if err := b.Store.SetApproved(r.Context(), id, true, me.Login); err != nil {
			slog.Warn("auto-approve", "item", id, "err", err)
		}
	}
	if len(tags) > 0 {
		if err := b.Store.SetTags(r.Context(), id, tags, false); err != nil {
			slog.Warn("tags", "item", id, "err", err)
		}
	}
	if err := b.saveUploads(r, id, me.Login); err != nil {
		slog.Warn("upload", "item", id, "err", err)
	}
	b.mention(r.Context(), id, me.Login, body)
	b.pushover(r.Context(), me.Login, "New ask from "+me.Login, title, b.itemURL(id))
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
		"CanClose":    me.IsAdmin() || strings.EqualFold(me.Login, it.Author),
		"Repo":        b.Repo,
		"CanEscalate": !isDone(it) && ((me.IsAdmin() && b.Platform != nil && it.Escalation != "platform") || (!me.IsAdmin() && it.Escalation == "")),
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
	// A merged item reopens once it ships, so the ship-checker keeps its merge.
	reopensOnShip := me.IsAdmin() && it.Status == "merged"
	if reopensOnShip {
		if err := b.Store.ReopenOnShip(r.Context(), id); err != nil {
			b.fail(w, err)
			return
		}
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
	b.pushover(r.Context(), me.Login, me.Login+" replied: "+it.Title, body, b.itemURL(id))
	b.Gate.Activity()
	msg := ""
	if reopened {
		msg = "Reopened"
	} else if reopensOnShip {
		msg = "Reopens once shipped"
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
		me := auth.Member(r.Context()).Login
		if it, err := b.Store.Item(r.Context(), id); err == nil {
			b.pushover(r.Context(), me, me+" approved: "+it.Title, "The agent will build it on its next run.", b.itemURL(id))
		}
		b.Gate.Activity()
	}
	redirect(w, r, fmt.Sprintf("/items/%d", id), msg)
}

// unblock clears an item's blocked flag; it's human activity, so the gate
// picks the item up again.
func (b *Board) unblock(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if err := b.Store.SetBlocked(r.Context(), id, "", true); err != nil {
		b.fail(w, err)
		return
	}
	b.Gate.Activity()
	redirect(w, r, fmt.Sprintf("/items/%d", id), "Unblocked")
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

func (b *Board) markAllRead(w http.ResponseWriter, r *http.Request) {
	if err := b.Store.MarkRead(r.Context(), auth.Member(r.Context()).Login, 0); err != nil {
		b.fail(w, err)
		return
	}
	http.Redirect(w, r, backPath(r.FormValue("back")), http.StatusSeeOther)
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
	var rel *releaseInfo
	if auth.Member(r.Context()).IsAdmin() {
		rel = b.releaseInfo(r.Context(), "/runs")
	}
	b.render(w, r, "runs", "Runs", map[string]any{"Runs": rows, "Release": rel})
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
	var upstream map[string]any
	if b.Upstream != "" {
		on, pr, err := b.Store.UpstreamSync(r.Context())
		if err != nil {
			b.fail(w, err)
			return
		}
		upstream = map[string]any{"Repo": b.Upstream, "On": on, "PR": pr}
	}
	workflows, err := b.Store.AgentWorkflows(r.Context())
	if err != nil {
		b.fail(w, err)
		return
	}
	stuck, err := b.stuckSignIns(r.Context(), members)
	if err != nil {
		b.fail(w, err)
		return
	}
	var mail map[string]any
	if b.Mailer != nil {
		mail = map[string]any{"From": b.Mailer.Mailbox(r)}
	}
	b.render(w, r, "admin", "Admin", map[string]any{"Members": members, "Stuck": stuck, "Hold": b.inviteHold(r.Context()), "Costs": costs, "Traffic": b.Traffic,
		"Limit": limit, "MemberCosts": byMember, "Stats": []db.RunStats{recent, all}, "Upstream": upstream, "Mail": mail, "HubLimits": b.HubLimits,
		"Workflows": workflows, "GitHub": b.gitHubSignIn()})
}

// gitHubSignIn reports whether members may sign in with GitHub at all.
func (b *Board) gitHubSignIn() bool { return b.Auth == nil || b.Auth.GitHub() }

func (b *Board) setSpendLimit(w http.ResponseWriter, r *http.Request) {
	if b.HubLimits {
		redirect(w, r, "/admin", "The hub's admins set this app's spend limit")
		return
	}
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

func (b *Board) setUpstreamSync(w http.ResponseWriter, r *http.Request) {
	on := r.FormValue("on") == "true"
	if err := b.Store.SetUpstreamSync(r.Context(), on); err != nil {
		b.fail(w, err)
		return
	}
	msg := "No longer keeping up with " + b.Upstream
	if on {
		msg = "Keeping up with " + b.Upstream + ": new commits merge within a tick or two once CI passes"
	}
	redirect(w, r, "/admin", msg)
}

func (b *Board) setAgentWorkflows(w http.ResponseWriter, r *http.Request) {
	on := r.FormValue("on") == "true"
	if err := b.Store.SetAgentWorkflows(r.Context(), on); err != nil {
		b.fail(w, err)
		return
	}
	msg := "The agent can no longer change workflows; it blocks and hands you the diff"
	if on {
		msg = "The agent may change workflows from the next run"
	}
	redirect(w, r, "/admin", msg)
}

func (b *Board) setAutoApprove(w http.ResponseWriter, r *http.Request) {
	me := auth.Member(r.Context())
	on := r.FormValue("on") == "true"
	if err := b.Store.SetAutoApprove(r.Context(), me.Login, on); err != nil {
		b.fail(w, err)
		return
	}
	msg := "Auto-approve off"
	if on {
		msg = "Auto-approve on"
	}
	redirect(w, r, "/admin", msg)
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
	email := strings.TrimSpace(r.FormValue("email"))
	if email != "" && !strings.Contains(email, "@") {
		redirect(w, r, "/admin", "That email doesn't look right")
		return
	}
	if email == "" && !b.gitHubSignIn() {
		redirect(w, r, "/admin", "Enter their Google email: GitHub sign-in is off")
		return
	}
	_, err := b.Store.Member(r.Context(), login)
	isNew := errors.Is(err, db.ErrNotFound)
	if err != nil && !isNew {
		b.fail(w, err)
		return
	}
	if err := b.Store.UpsertMember(r.Context(), login, role, auth.Member(r.Context()).Login); err != nil {
		b.fail(w, err)
		return
	}
	github := email == "" || r.FormValue("github") == "on"
	if email != "" {
		if err := b.Store.SetMemberEmail(r.Context(), login, email); errors.Is(err, db.ErrEmailTaken) {
			redirect(w, r, "/admin", "Saved "+login+", but "+email+" belongs to another member")
			return
		} else if err != nil {
			b.fail(w, err)
			return
		}
		if err := b.Store.SetMemberGitHub(r.Context(), login, github); err != nil {
			b.fail(w, err)
			return
		}
	}
	if hold := b.inviteHold(r.Context()); isNew && hold != "" {
		redirect(w, r, "/admin", "Saved "+login+"; invite them once it's ready: "+hold)
		return
	}
	if isNew && email == "" && b.Welcomer != nil {
		// No email to invite them by, and GitHub has no DMs: an @-mention notifies them.
		n, err := b.Welcomer.Welcome(r.Context(), b.name(), login)
		if err != nil {
			slog.Warn("welcome member", "login", login, "err", err)
			redirect(w, r, "/admin", "Saved "+login+", but couldn't welcome them on GitHub; send an Invite instead")
			return
		}
		redirect(w, r, "/admin", fmt.Sprintf("Saved %s and welcomed them on GitHub issue #%d", login, n))
		return
	}
	redirect(w, r, "/admin", "Saved "+login)
}

// setMemberGitHub says whether a member with an email may also sign in with
// GitHub as their login.
func (b *Board) setMemberGitHub(w http.ResponseWriter, r *http.Request) {
	login := r.PathValue("login")
	on := r.FormValue("on") == "true"
	if err := b.Store.SetMemberGitHub(r.Context(), login, on); errors.Is(err, db.ErrNotFound) {
		http.NotFound(w, r)
		return
	} else if err != nil {
		b.fail(w, err)
		return
	}
	msg := login + " signs in with Google only"
	if on {
		msg = login + " signs in with GitHub or Google"
	}
	redirect(w, r, "/admin", msg)
}

// setMemberEmail changes, or with an empty email clears, the address a member
// signs in with Google as; without one they sign in with GitHub only.
func (b *Board) setMemberEmail(w http.ResponseWriter, r *http.Request) {
	m, err := b.Store.Member(r.Context(), r.PathValue("login"))
	if errors.Is(err, db.ErrNotFound) {
		http.NotFound(w, r)
		return
	} else if err != nil {
		b.fail(w, err)
		return
	}
	// Google sign-in matches the lower-cased address.
	email := strings.ToLower(strings.TrimSpace(r.FormValue("email")))
	switch {
	case email == m.Email:
		redirect(w, r, "/admin", m.Login+"'s email is unchanged")
		return
	case email != "" && !strings.Contains(email, "@"):
		redirect(w, r, "/admin", "That email doesn't look right")
		return
	case email == "" && !b.gitHubSignIn():
		redirect(w, r, "/admin", "GitHub sign-in is off, so "+m.Login+" needs a Google email")
		return
	}
	if err := b.Store.SetMemberEmail(r.Context(), m.Login, email); errors.Is(err, db.ErrEmailTaken) {
		redirect(w, r, "/admin", email+" belongs to another member")
		return
	} else if err != nil {
		b.fail(w, err)
		return
	}
	if email == "" {
		// GitHub is then their only way in.
		if err := b.Store.SetMemberGitHub(r.Context(), m.Login, true); err != nil {
			b.fail(w, err)
			return
		}
		redirect(w, r, "/admin", "Cleared "+m.Login+"'s email: they sign in with GitHub as "+m.Login+" only, so that must be their GitHub login")
		return
	}
	msg := m.Login + "'s email is now " + email + ": they sign in with Google as " + email
	if m.Email != "" {
		msg = m.Login + "'s email changed from " + m.Email + " to " + email + ": they sign in with Google as " + email
	}
	if m.LastSeen == nil {
		msg += "; their invite goes there too"
	}
	redirect(w, r, "/admin", msg)
}

// signIns lists the last 30 days' sign-in attempts at this board, plus
// failures anywhere by a member's login or email; ?who=<login> just theirs.
func (b *Board) signIns(w http.ResponseWriter, r *http.Request) {
	members, err := b.Store.Members(r.Context())
	if err != nil {
		b.fail(w, err)
		return
	}
	who := r.URL.Query().Get("who")
	var ids []string
	for _, m := range members {
		if who != "" && !strings.EqualFold(m.Login, who) {
			continue
		}
		ids = append(ids, m.Login)
		if m.Email != "" {
			ids = append(ids, m.Email)
		}
	}
	if who != "" && len(ids) == 0 {
		http.NotFound(w, r)
		return
	}
	list, err := b.signInLog().SignIns(r.Context(), b.Slug, ids, who != "", time.Now().Add(-30*24*time.Hour))
	if err != nil {
		b.fail(w, err)
		return
	}
	b.render(w, r, "signins", "Sign-ins", map[string]any{"SignIns": list, "Who": who, "Slug": b.Slug})
}

// signInLog is where this board's sign-in attempts are: SignIns, else the store.
func (b *Board) signInLog() db.SignInLog {
	if b.SignIns != nil {
		return b.SignIns
	}
	return b.Store
}

// stuckWithin is how far back /admin looks for never-signed-in members' failures.
const stuckWithin = 7 * 24 * time.Hour

// StuckSignIn is a member who has never signed in and failed to since being
// added: usually the wrong address or provider.
type StuckSignIn struct {
	Login string
	Last  db.SignIn // the latest failure
}

// stuckSignIns lists members who have never signed in but failed to in the
// last week (after they were added), with their latest failure, one per member.
func (b *Board) stuckSignIns(ctx context.Context, members []db.Member) ([]StuckSignIn, error) {
	owner := map[string]db.Member{}
	var ids []string
	for _, m := range members {
		if m.LastSeen != nil {
			continue
		}
		owner[strings.ToLower(m.Login)] = m
		ids = append(ids, m.Login)
		if m.Email != "" {
			owner[strings.ToLower(m.Email)] = m
			ids = append(ids, m.Email)
		}
	}
	if len(ids) == 0 {
		return nil, nil
	}
	list, err := b.signInLog().SignIns(ctx, b.Slug, ids, true, time.Now().Add(-stuckWithin))
	if err != nil {
		return nil, err
	}
	var out []StuckSignIn
	seen := map[string]bool{}
	for _, s := range list { // newest first
		m, ok := owner[strings.ToLower(s.Identity)]
		if !ok || !s.Failed() || s.At.Before(m.AddedAt) || seen[m.Login] {
			continue
		}
		seen[m.Login] = true
		out = append(out, StuckSignIn{Login: m.Login, Last: s})
	}
	return out, nil
}

// inviteHold is why invites wait, or "" when they needn't.
func (b *Board) inviteHold(ctx context.Context) string {
	if b.InviteHold == nil {
		return ""
	}
	return b.InviteHold(ctx)
}

// name is the board's name for invitations: b.Name, else BaseURL's host.
func (b *Board) name() string {
	if b.Name != "" {
		return b.Name
	}
	u, _ := url.Parse(b.BaseURL)
	if u == nil {
		return ""
	}
	return u.Host
}

// inviteMessage tells m they're a member and how to sign in.
func (b *Board) inviteMessage(m db.Member) (subject, body string) {
	name := b.name()
	how := "Sign in with GitHub as " + m.Login + "."
	switch {
	case m.Email != "" && m.GitHub && b.gitHubSignIn():
		how = "Sign in with GitHub as " + m.Login + " or with Google as " + m.Email + "."
	case m.Email != "":
		how = "Sign in with Google as " + m.Email + "."
	}
	return "You're on the " + name + " board",
		"I added you to the " + name + " board, where you can ask for changes and follow them as they're built:\n\n" + strings.TrimSuffix(b.BaseURL, "/") + "/\n\n" + how + "\n"
}

// invite is a mailto: link with m's invitation, addressed to m.Email when
// set (else the admin fills in the address).
func (b *Board) invite(m db.Member) string {
	subject, body := b.inviteMessage(m)
	q := url.Values{"subject": {subject}, "body": {body}}
	// mailto wants %20, not +, for spaces.
	return "mailto:" + url.PathEscape(m.Email) + "?" + strings.ReplaceAll(q.Encode(), "+", "%20")
}

// sendInvite emails m's invitation from the admin's connected Gmail.
func (b *Board) sendInvite(w http.ResponseWriter, r *http.Request) {
	if b.Mailer == nil {
		http.Error(w, "mail is not configured", http.StatusServiceUnavailable)
		return
	}
	m, err := b.Store.Member(r.Context(), r.PathValue("login"))
	if err != nil {
		b.fail(w, err)
		return
	}
	if m.Email == "" {
		redirect(w, r, "/admin", m.Login+" has no email; use the Invite link")
		return
	}
	if hold := b.inviteHold(r.Context()); hold != "" {
		redirect(w, r, "/admin", "Not yet: "+hold)
		return
	}
	subject, body := b.inviteMessage(m)
	from, err := b.Mailer.Mail(r, m.Email, subject, body)
	if err != nil {
		slog.Warn("invite mail", "to", m.Email, "err", err)
		redirect(w, r, "/admin", "Couldn't email "+m.Email+": "+err.Error()+"; try reconnecting Gmail")
		return
	}
	redirect(w, r, "/admin", "Emailed an invite to "+m.Email+" from "+from)
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
	case errors.Is(err, db.ErrHeld):
		redirect(w, r, "/runs", "Held: "+strings.TrimPrefix(err.Error(), db.ErrHeld.Error()+": "))
	case err != nil:
		slog.Error("run now", "err", err)
		redirect(w, r, "/runs", "Launch failed: "+err.Error())
	default:
		redirect(w, r, "/runs", fmt.Sprintf("Started run %d", id))
	}
}

func (b *Board) cancelRun(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	switch err := b.Gate.Cancel(r.Context(), id, auth.Member(r.Context()).Login); {
	case errors.Is(err, db.ErrNotFound):
		redirect(w, r, "/runs", fmt.Sprintf("Run %d already finished", id))
	case err != nil:
		slog.Error("cancel run", "run", id, "err", err)
		redirect(w, r, "/runs", err.Error())
	default:
		redirect(w, r, "/runs", fmt.Sprintf("Cancelled run %d", id))
	}
}

// prefix is the path the board is mounted at, from BaseURL: "/iterate", or "" at the root.
func (b *Board) prefix() string {
	u, err := url.Parse(b.BaseURL)
	if err != nil {
		return ""
	}
	return strings.TrimSuffix(u.Path, "/")
}

// pushover sends board activity by anyone but Quiet.
func (b *Board) pushover(ctx context.Context, actor, title, msg, url string) {
	if strings.EqualFold(actor, b.Quiet) {
		return
	}
	b.Pushover.Send(context.WithoutCancel(ctx), title, msg, url)
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
	b.render(w, r, "login", "Sign in", map[string]any{"Next": next, "GitHub": b.Auth.GitHub(), "Google": b.Auth.Google()})
}
