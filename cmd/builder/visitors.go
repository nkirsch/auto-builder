package main

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nkirsch/auto-builder/internal/gate"
	"github.com/nkirsch/auto-builder/internal/github"
	"github.com/nkirsch/auto-builder/internal/tenant"
)

// Visitors: each reader of an app's site, from the same request log views and
// cache as /logs, with the version (release and the PRs it shipped) each page
// view was served. A board's admins see their own app's at /<slug>/visitors;
// platform admins see every app's at /visitors (visitors.<APP_DOMAIN>
// redirects there). Pseudonymous by design: a visitor is a keyed hash of IP +
// user agent, scoped to the board (or the platform) so IDs can't be matched
// across boards; no IP, user agent, sign-in or member data reaches the page.
// Admins' own visits (any IP a platform admin or the board's admin signed in
// from) and HOME_HOST's IPs are left out.

const (
	visitorBurst     = 10 // this many page views within a minute is a crawler
	visitorRows      = 300
	visitorTimeline  = 60
	versionsTTL      = time.Hour
	visitorsParallel = 3                // apps read at once, under Logging's per-minute read quota
	versionsRetry    = 10 * time.Minute // after a failed revision lookup
	adminIPDays      = 90               // sign-ins are kept 90 days
)

// version names what a revision served: its release tag (else the short SHA)
// and the titles of the PRs it shipped since the revision before.
type version struct {
	Label   string
	SHA     string
	Changes []string
	Created time.Time
}

// versionLabels labels each revision from the repo's history. commits are
// newest first; a revision's changes are the PRs merged after the previous
// revision's (different) SHA, up to and including its own.
func versionLabels(revs []gate.Revision, commits []github.Commit, tags map[string][]string, prs []github.MergedPR) map[string]version {
	idx := map[string]int{}
	for i, c := range commits {
		idx[c.SHA] = i
	}
	bySHA := map[string][]string{}
	for _, p := range prs {
		bySHA[p.SHA] = append(bySHA[p.SHA], p.Title)
	}
	revs = slices.Clone(revs)
	sort.Slice(revs, func(i, j int) bool { return revs[i].Created.Before(revs[j].Created) })
	out := map[string]version{}
	prev := ""
	for _, r := range revs {
		v := version{SHA: r.SHA, Created: r.Created, Label: r.Name}
		if r.SHA != "" {
			v.Label = r.SHA[:min(7, len(r.SHA))]
			if t := tags[r.SHA]; len(t) > 0 {
				t = slices.Clone(t)
				slices.Sort(t)
				v.Label = t[len(t)-1]
			}
			hi, ok := idx[r.SHA]
			lo, prevOK := idx[prev]
			switch {
			case r.SHA == prev:
				// a redeploy of the same commit ships nothing new
			case ok && prevOK && lo > hi:
				for i := hi; i < lo; i++ {
					v.Changes = append(v.Changes, bySHA[commits[i].SHA]...)
				}
			default:
				v.Changes = bySHA[r.SHA]
			}
			prev = r.SHA
		}
		out[r.Name] = v
	}
	return out
}

// versioner caches one app's revision labels.
type versioner struct {
	revisions func(context.Context) ([]gate.Revision, error)
	history   func(context.Context) ([]github.Commit, map[string][]string, []github.MergedPR, error)

	mu     sync.Mutex
	at     time.Time
	labels map[string]version
}

func (v *versioner) labelsFor(ctx context.Context) map[string]version {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.labels != nil && time.Since(v.at) < versionsTTL {
		return v.labels
	}
	labels := map[string]version{}
	revs, err := v.revisions(ctx)
	if err != nil {
		// Unlabeled (the page shows revision names) until versionsRetry passes,
		// so an app whose service can't be read doesn't cost a lookup per page.
		slog.Warn("visitors revisions", "err", err)
		v.labels, v.at = labels, time.Now().Add(versionsRetry-versionsTTL)
		return labels
	}
	var (
		commits []github.Commit
		tags    map[string][]string
		prs     []github.MergedPR
	)
	if v.history != nil {
		if commits, tags, prs, err = v.history(ctx); err != nil {
			slog.Warn("visitors history", "err", err)
		}
	}
	v.labels, v.at = versionLabels(revs, commits, tags, prs), time.Now()
	return v.labels
}

// versions is slug's versioner for the Cloud Run service its log entries name.
func (h *hub) versions(slug, service string) *versioner {
	h.trafficMu.Lock()
	defer h.trafficMu.Unlock()
	if v, ok := h.versioners[slug+" "+service]; ok {
		return v
	}
	v := &versioner{revisions: func(ctx context.Context) ([]gate.Revision, error) {
		p := strings.Split(service, "/") // projects/P/locations/R/services/S
		if len(p) != 6 {
			return nil, fmt.Errorf("service %q", service)
		}
		s, err := gate.NewServiceSHA(ctx, p[1], p[3], p[5])
		if err != nil {
			return nil, err
		}
		return s.Revisions(ctx, 5)
	}}
	if h.app != nil {
		// Resolved per read, not here: tenantFor takes h.mu and h.trafficMu.
		v.history = func(ctx context.Context) ([]github.Commit, map[string][]string, []github.MergedPR, error) {
			rt, err := h.tenantFor(ctx, slug)
			if err != nil {
				return nil, nil, nil, err
			}
			gh, repo := rt.b.GitHub, rt.b.Repo
			commits, err := gh.Commits(ctx, repo, 100)
			if err != nil {
				return nil, nil, nil, err
			}
			tags, err := gh.CommitTags(ctx, repo, "v")
			if err != nil {
				return nil, nil, nil, err
			}
			prs, err := gh.MergedPRs(ctx, repo, 100)
			return commits, tags, prs, err
		}
	}
	if h.versioners == nil {
		h.versioners = map[string]*versioner{}
	}
	h.versioners[slug+" "+service] = v
	return v
}

// appHits is one app's cached hits and what to leave out of them.
type appHits struct {
	App     string // slug
	Host    string // <slug>.<domain>
	Hits    []hit
	Exclude map[string]bool    // IPs: admins' and the operator's home network
	Labels  map[string]version // by revision; a missing one shows its revision name
}

type visitorHit struct {
	At                 time.Time
	App, Path, Version string
}

type visitorRow struct {
	ID, Browser string
	First, Last time.Time
	Views       int
	Apps        []string
	Versions    []string // labels seen, first seen first
	Referrers   []string // external hosts
	Timeline    []visitorHit
	More        int // timeline entries left out
}

type versionRow struct {
	App, Label string
	Changes    []string
	Created    time.Time
	Views      int
	Visitors   int
	who        map[string]bool
}

type visitorsReport struct {
	Scope    string // the app's host, or "every app"
	All      bool   // platform view: show each row's apps
	From, To time.Time
	Views    int
	Total    int // visitors, before visitorRows
	Rows     []visitorRow
	Versions []versionRow
	Dropped  int      // page views from bots, crawlers, admins and the home network
	Errs     []string // apps whose log couldn't be read
	Reading  []string // apps whose log is partly read; a reload continues
}

// pseudonym is a short keyed hash of IP + user agent, scoped so the same
// browser has different IDs on different boards.
func pseudonym(key []byte, scope, ip, ua string) string {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(scope + "\x00" + ip + "\x00" + ua))
	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(m.Sum(nil)[:5]))
}

// buildVisitors groups page views by pseudonym. A visitor is a browser (it
// fetched CSS or JS from that app) that isn't a declared bot, an excluded IP,
// or a crawler (visitorBurst views within a minute).
func buildVisitors(apps []appHits, key []byte, scope string, hide func(string) bool) visitorsReport {
	r := visitorsReport{}
	rows := map[string]*visitorRow{}
	versions := map[string]*versionRow{}
	for _, a := range apps {
		browserOf := map[string]bool{}
		for _, h := range a.Hits {
			if assetRe.MatchString(h.Path) && (h.Status == 200 || h.Status == 304) {
				browserOf[h.IP+"|"+h.UA] = true
			}
		}
		type view struct {
			h  hit
			id string
		}
		byWho := map[string][]view{}
		for _, h := range a.Hits {
			if !page(h) {
				continue
			}
			if botUA.MatchString(h.UA) || a.Exclude[h.IP] || !browserOf[h.IP+"|"+h.UA] {
				r.Dropped++
				continue
			}
			byWho[h.IP+"|"+h.UA] = append(byWho[h.IP+"|"+h.UA], view{h, pseudonym(key, scope, h.IP, h.UA)})
		}
		for _, vs := range byWho {
			sort.Slice(vs, func(i, j int) bool { return vs[i].h.At.Before(vs[j].h.At) })
			if burst(vs, func(v view) time.Time { return v.h.At }) {
				r.Dropped += len(vs)
				continue
			}
			for _, v := range vs {
				x := rows[v.id]
				if x == nil {
					x = &visitorRow{ID: v.id, Browser: browser(v.h.UA), First: v.h.At}
					rows[v.id] = x
				}
				ver, ok := a.Labels[v.h.Revision]
				if !ok {
					ver = version{Label: v.h.Revision}
				}
				if ver.Label == "" {
					ver.Label = "unknown"
				}
				x.Views++
				r.Views++
				x.First, x.Last = minTime(x.First, v.h.At), maxTime(x.Last, v.h.At)
				addOnce(&x.Apps, a.App)
				addOnce(&x.Versions, ver.Label)
				if ref := refHost(v.h.Ref); ref != "" && ref != a.Host && !hide(ref) {
					addOnce(&x.Referrers, ref)
				}
				x.Timeline = append(x.Timeline, visitorHit{At: v.h.At, App: a.App, Path: v.h.Path, Version: ver.Label})
				vk := a.App + " " + ver.Label
				vr := versions[vk]
				if vr == nil {
					vr = &versionRow{App: a.App, Label: ver.Label, Changes: ver.Changes, Created: ver.Created, who: map[string]bool{}}
					versions[vk] = vr
				}
				vr.Views++
				vr.who[v.id] = true
			}
		}
	}
	for _, x := range rows {
		sort.SliceStable(x.Timeline, func(i, j int) bool { return x.Timeline[i].At.After(x.Timeline[j].At) })
		if n := len(x.Timeline); n > visitorTimeline {
			x.Timeline, x.More = x.Timeline[:visitorTimeline], n-visitorTimeline
		}
		r.Rows = append(r.Rows, *x)
	}
	sort.Slice(r.Rows, func(i, j int) bool { return r.Rows[i].Last.After(r.Rows[j].Last) })
	r.Total = len(r.Rows)
	r.Rows = r.Rows[:min(len(r.Rows), visitorRows)]
	for _, v := range versions {
		v.Visitors = len(v.who)
		r.Versions = append(r.Versions, *v)
	}
	sort.Slice(r.Versions, func(i, j int) bool {
		if !r.Versions[i].Created.Equal(r.Versions[j].Created) {
			return r.Versions[i].Created.After(r.Versions[j].Created)
		}
		return r.Versions[i].App+r.Versions[i].Label < r.Versions[j].App+r.Versions[j].Label
	})
	return r
}

// burst reports whether visitorBurst of the (sorted) views fall within a minute.
func burst[T any](vs []T, at func(T) time.Time) bool {
	for i, j := 0, 0; j < len(vs); j++ {
		for at(vs[j]).Sub(at(vs[i])) >= time.Minute {
			i++
		}
		if j-i+1 >= visitorBurst {
			return true
		}
	}
	return false
}

func refHost(ref string) string {
	u, err := url.Parse(ref)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

func addOnce(list *[]string, s string) {
	if !slices.Contains(*list, s) {
		*list = append(*list, s)
	}
}

func minTime(a, b time.Time) time.Time {
	if a.IsZero() || b.Before(a) {
		return b
	}
	return a
}

func maxTime(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// hiddenReferrer reports whether a referring host is the platform's own (the
// hub, any app), so one board's page never names another app.
func (h *hub) hiddenReferrer(host string) bool {
	under := func(d string) bool { return d != "" && (host == d || strings.HasSuffix(host, "."+d)) }
	if u, err := url.Parse(h.cfg.BaseURL); err == nil && under(u.Hostname()) {
		return true
	}
	return under(h.host)
}

// visitorKey is the pseudonyms' key, from SESSION_KEY so IDs hold across
// restarts; random (IDs change per instance) without one.
func (h *hub) visitorKey() []byte {
	h.trafficMu.Lock()
	defer h.trafficMu.Unlock()
	if h.vkey == nil {
		if h.cfg.SessionKey != "" {
			m := hmac.New(sha256.New, []byte(h.cfg.SessionKey))
			m.Write([]byte("visitor pseudonyms"))
			h.vkey = m.Sum(nil)
		} else {
			h.vkey = make([]byte, 32)
			rand.Read(h.vkey)
		}
	}
	return h.vkey
}

// adminIPs are the IPs slug's admins and the platform admins signed in from.
func (h *hub) adminIPs(ctx context.Context, slug string) (map[string]bool, error) {
	var ids []string
	pas, err := h.reg.PlatformAdmins(ctx)
	if err != nil {
		return nil, err
	}
	for _, a := range pas {
		ids = append(ids, a.Login, a.Email)
	}
	admins, err := h.reg.BoardAdmins(ctx, slug)
	if err != nil {
		return nil, err
	}
	ids = append(ids, admins...)
	ips, err := h.reg.SignInIPs(ctx, ids, time.Now().AddDate(0, 0, -adminIPDays))
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, ip := range ips {
		out[ip] = true
	}
	return out, nil
}

// visitorsReport reads slugs' hits (in parallel, through each /logs cache)
// and builds the page; scope keys the pseudonyms.
func (h *hub) visitorsReport(ctx context.Context, slugs []string, scope string, refresh bool) visitorsReport {
	apps := make([]appHits, len(slugs))
	errs := make([]string, len(slugs))
	var from time.Time
	var wg sync.WaitGroup
	var fromMu sync.Mutex
	sem := make(chan struct{}, visitorsParallel)
	for i, slug := range slugs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			tr := h.traffic(slug)
			hits, f, err := tr.recent(ctx, refresh)
			if err == nil {
				var ex map[string]bool
				if ex, err = h.adminIPs(ctx, slug); err == nil {
					for ip := range tr.home(ctx) {
						ex[ip] = true
					}
					a := appHits{App: slug, Host: tr.host, Hits: hits, Exclude: ex}
					svc := ""
					for _, x := range hits {
						if x.Service != "" {
							svc = x.Service
							break
						}
					}
					if svc != "" {
						a.Labels = h.versions(slug, svc).labelsFor(ctx)
					}
					apps[i] = a
					fromMu.Lock()
					if from.IsZero() || f.Before(from) {
						from = f
					}
					fromMu.Unlock()
					return
				}
			}
			slog.Warn("visitors", "app", slug, "err", err)
			errs[i] = slug
			if errors.As(err, new(errReading)) {
				errs[i] = "~" + slug
			}
		}()
	}
	wg.Wait()
	r := buildVisitors(apps, h.visitorKey(), scope, h.hiddenReferrer)
	for _, e := range errs {
		if slug, ok := strings.CutPrefix(e, "~"); ok {
			r.Reading = append(r.Reading, slug)
		} else if e != "" {
			r.Errs = append(r.Errs, e)
		}
	}
	r.From, r.To = from, time.Now()
	return r
}

// visitorsAll is /visitors: every ready app for platform admins (?app= narrows
// to one); a board admin goes to their app's /<slug>/visitors, or picks one.
func (h *hub) visitorsAll(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	me := h.auth.SignedIn(r)
	tenants, err := h.reg.Tenants(ctx)
	if err != nil {
		slog.Error("tenants", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if _, err := h.reg.PlatformAdmin(ctx, me); err != nil {
		var mine []tenant.Tenant
		for _, t := range tenants {
			if t.Status != "ready" {
				continue
			}
			if st, err := h.reg.Store(ctx, t.Slug); err == nil && roleOf(ctx, st, me) == "admin" {
				mine = append(mine, t)
			}
		}
		switch len(mine) {
		case 0:
			http.Error(w, "admins only", http.StatusForbidden)
		case 1:
			http.Redirect(w, r, "/"+mine[0].Slug+"/visitors", http.StatusFound)
		default:
			h.render(w, r, "visitors", "Visitors", map[string]any{"Pick": mine})
		}
		return
	}
	var slugs []string
	app := r.URL.Query().Get("app")
	for _, t := range tenants {
		if t.Status == "ready" && (app == "" || t.Slug == app) {
			slugs = append(slugs, t.Slug)
		}
	}
	rep := h.visitorsReport(ctx, slugs, "*", r.URL.Query().Has("refresh"))
	rep.Scope, rep.All = "every app", true
	if app != "" {
		rep.Scope = app
	}
	w.Header().Set("Cache-Control", "private, no-store")
	h.render(w, r, "visitors", "Visitors", map[string]any{"Report": rep, "Tenants": tenants, "App": app})
}

// visitorsFor is a board's /visitors: its own app only, behind RequireAdmin.
func (h *hub) visitorsFor(slug string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
		defer cancel()
		rep := h.visitorsReport(ctx, []string{slug}, slug, r.URL.Query().Has("refresh"))
		rep.Scope = h.traffic(slug).host
		w.Header().Set("Cache-Control", "private, no-store")
		h.render(w, r, "visitors", "Visitors", map[string]any{"Report": rep, "Board": slug})
	})
}

// visitorsHost sends visitors.<APP_DOMAIN> to the hub's /visitors, where the
// session cookie lives.
func (h *hub) visitorsHost(next http.Handler) http.Handler {
	host := "visitors." + h.host
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host == host && r.URL.Path != "/health" {
			http.Redirect(w, r, strings.TrimRight(h.cfg.BaseURL, "/")+"/visitors", http.StatusFound)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// browser gives a short label for a user agent: all the page shows of it.
func browser(ua string) string {
	name := "Other"
	for _, b := range []struct{ token, name string }{
		{"FBAN", "Facebook app"}, {"FB_IAB", "Facebook app"}, {"Instagram", "Instagram app"},
		{"Edg/", "Edge"}, {"Firefox/", "Firefox"}, {"CriOS/", "Chrome"}, {"Chrome/", "Chrome"}, {"Safari/", "Safari"},
	} {
		if strings.Contains(ua, b.token) {
			name = b.name
			break
		}
	}
	if name == "Other" && strings.Contains(ua, "AppleWebKit/") && strings.Contains(ua, "Mobile/") {
		name = "In-app"
	}
	for _, o := range []struct{ token, name string }{
		{"iPhone", "iPhone"}, {"iPad", "iPad"}, {"Android", "Android"}, {"Mac OS X", "Mac"}, {"Windows", "Windows"}, {"Linux", "Linux"},
	} {
		if strings.Contains(ua, o.token) {
			return name + " · " + o.name
		}
	}
	return name
}
