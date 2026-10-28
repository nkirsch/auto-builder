package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// The admin-only /logs page: who reads the static site, from Cloud Run's request
// log (read through a log view scoped to this service; see infra/bootstrap.sh).
// On a hub, each board's /logs reads its app's view, <slug>-requests.

const (
	trafficDays  = 30
	trafficCache = time.Hour
	homeHost     = "home.kirsch.org" // Nick's dynamic DNS; his own visits are dropped
)

var (
	pacific = mustZone("America/Los_Angeles")
	botUA   = regexp.MustCompile(`(?i)bot\b|bot/|crawl|spider|slurp|fetcher|go-http|python|curl/|libwww|wget|scrapy|okhttp|java/|node-fetch|axios|headless|lighthouse|facebookexternal|google-certificates`)
	assetRe = regexp.MustCompile(`\.(css|js)$`)
)

func mustZone(name string) *time.Location {
	loc, err := time.LoadLocation(name)
	if err != nil {
		return time.UTC
	}
	return loc
}

type hit struct {
	At                time.Time
	IP, UA, Path, Ref string
	Method            string
	Status            int
}

type count struct {
	Key             string
	Views, Visitors int
}

type trafficReport struct {
	From, To         time.Time
	Views, Visitors  int
	Dropped          int // requests from bots, scrapers and Nick
	Days             []count
	Pages, Referrers []count
	Max              int // busiest day's views, for the bars
}

// page is a site page view: not the board, an asset, or a probe.
func page(h hit) bool {
	return h.Method == http.MethodGet && (h.Status == 200 || h.Status == 304) &&
		!strings.HasPrefix(h.Path, "/iterate") && !strings.HasPrefix(h.Path, "/.well-known") &&
		(strings.HasSuffix(h.Path, "/") || strings.HasSuffix(h.Path, ".html"))
}

// buildReport counts real readers: a visitor is an IP+UA that isn't a declared
// bot, isn't excluded, and fetched a stylesheet or script as browsers do.
func buildReport(hits []hit, host string, from, to time.Time, exclude map[string]bool) trafficReport {
	browser := map[string]bool{}
	for _, h := range hits {
		if assetRe.MatchString(h.Path) {
			browser[h.IP+"|"+h.UA] = true
		}
	}
	r := trafficReport{From: from, To: to}
	visitors := map[string]bool{}
	days, pages, refs := map[string]*tally{}, map[string]*tally{}, map[string]*tally{}
	for d := from.In(pacific); !d.After(to); d = d.AddDate(0, 0, 1) {
		days[d.Format("2006-01-02")] = &tally{}
	}
	for _, h := range hits {
		if !page(h) {
			continue
		}
		who := h.IP + "|" + h.UA
		if botUA.MatchString(h.UA) || exclude[h.IP] || !browser[who] {
			r.Dropped++
			continue
		}
		r.Views++
		visitors[who] = true
		add(days, h.At.In(pacific).Format("2006-01-02"), who)
		add(pages, h.Path, who)
		if u, err := url.Parse(h.Ref); err == nil && u.Host != "" && u.Host != host {
			add(refs, u.Host, who)
		}
	}
	r.Visitors = len(visitors)
	for k, t := range days {
		r.Days = append(r.Days, count{k, t.views, len(t.who)})
		r.Max = max(r.Max, t.views)
	}
	sort.Slice(r.Days, func(i, j int) bool { return r.Days[i].Key > r.Days[j].Key })
	r.Pages, r.Referrers = top(pages, 30), top(refs, 20)
	return r
}

type tally struct {
	views int
	who   map[string]bool
}

func add(m map[string]*tally, key, who string) {
	t := m[key]
	if t == nil {
		t = &tally{}
		m[key] = t
	}
	if t.who == nil {
		t.who = map[string]bool{}
	}
	t.views++
	t.who[who] = true
}

func top(m map[string]*tally, n int) []count {
	var out []count
	for k, t := range m {
		out = append(out, count{k, t.views, len(t.who)})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Views != out[j].Views {
			return out[i].Views > out[j].Views
		}
		return out[i].Key < out[j].Key
	})
	return out[:min(n, len(out))]
}

// logView reads request entries through a Cloud Logging view with the
// metadata server's token, so the server needs no client library.
type logView struct {
	name   string // projects/P/locations/global/buckets/_Default/views/V
	client *http.Client
}

func (l logView) hits(ctx context.Context, since time.Time) ([]hit, error) {
	tok, err := metadataToken(ctx, l.client)
	if err != nil {
		return nil, err
	}
	var out []hit
	body := map[string]any{
		"resourceNames": []string{l.name},
		"filter":        fmt.Sprintf(`timestamp >= %q AND httpRequest.requestMethod = "GET"`, since.UTC().Format(time.RFC3339)),
		"orderBy":       "timestamp asc",
		"pageSize":      1000,
	}
	for pages := 0; pages < 100; pages++ {
		b, _ := json.Marshal(body)
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost,
			"https://logging.googleapis.com/v2/entries:list?fields=entries(timestamp,httpRequest),nextPageToken", bytes.NewReader(b))
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("Content-Type", "application/json")
		resp, err := l.client.Do(req)
		if err != nil {
			return nil, err
		}
		var page struct {
			Entries []struct {
				Timestamp   time.Time `json:"timestamp"`
				HTTPRequest struct {
					RequestMethod, RequestURL, UserAgent, RemoteIP, Referer string
					Status                                                  int
				} `json:"httpRequest"`
			} `json:"entries"`
			NextPageToken string `json:"nextPageToken"`
		}
		err = json.NewDecoder(resp.Body).Decode(&page)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("entries.list: %s", resp.Status)
		}
		if err != nil {
			return nil, err
		}
		for _, e := range page.Entries {
			h := e.HTTPRequest
			p := h.RequestURL
			if u, err := url.Parse(h.RequestURL); err == nil {
				p = u.Path
			}
			out = append(out, hit{At: e.Timestamp, IP: h.RemoteIP, UA: h.UserAgent, Path: p,
				Ref: h.Referer, Method: h.RequestMethod, Status: h.Status})
		}
		if page.NextPageToken == "" {
			return out, nil
		}
		body["pageToken"] = page.NextPageToken
	}
	return out, nil
}

func metadataToken(ctx context.Context, c *http.Client) (string, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet,
		"http://metadata.google.internal/computeMetadata/v1/instance/service-accounts/default/token", nil)
	req.Header.Set("Metadata-Flavor", "Google")
	resp, err := c.Do(req)
	if err != nil {
		return "", fmt.Errorf("metadata token: %w", err)
	}
	defer resp.Body.Close()
	var t struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&t); err != nil || t.AccessToken == "" {
		return "", fmt.Errorf("metadata token: %s", resp.Status)
	}
	return t.AccessToken, nil
}

// traffic serves the report, rebuilt at most once per trafficCache.
type traffic struct {
	host  string
	view  string // the log view read, "" for none
	hits  func(ctx context.Context, since time.Time) ([]hit, error)
	home  func(ctx context.Context) map[string]bool
	now   func() time.Time
	mu    sync.Mutex
	at    time.Time
	cache []byte
}

func newTraffic(baseURL, project, service string) *traffic {
	u, _ := url.Parse(baseURL)
	if project == "" || service == "" {
		return trafficFrom(u.Host, "")
	}
	return trafficFrom(u.Host, logViewName(project, service+"-requests"))
}

// trafficFrom reads hits through the named log view; "" when there's none.
func trafficFrom(host, view string) *traffic {
	t := &traffic{host: host, view: view, now: time.Now, home: lookupHome}
	t.hits = logView{name: view, client: &http.Client{Timeout: 30 * time.Second}}.hits
	if view == "" {
		t.hits = func(context.Context, time.Time) ([]hit, error) {
			return nil, fmt.Errorf("no request log view (needs GCP_PROJECT, and K_SERVICE or LOG_PROJECT)")
		}
	}
	return t
}

// traffic is a hub board's /logs: its app's view <slug>-requests in LOG_PROJECT.
func (h *hub) traffic(slug string) *traffic {
	u, _ := url.Parse(h.appURL(slug))
	if h.cfg.LogProject == "" {
		return trafficFrom(u.Host, "")
	}
	return trafficFrom(u.Host, logViewName(h.cfg.LogProject, slug+"-requests"))
}

func logViewName(project, view string) string {
	return fmt.Sprintf("projects/%s/locations/global/buckets/_Default/views/%s", project, view)
}

func lookupHome(ctx context.Context) map[string]bool {
	ips, _ := net.DefaultResolver.LookupHost(ctx, homeHost)
	m := map[string]bool{}
	for _, ip := range ips {
		m[ip] = true
	}
	return m
}

func (t *traffic) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	if t.cache == nil || now.Sub(t.at) > trafficCache || r.URL.Query().Has("refresh") {
		from := time.Date(now.In(pacific).Year(), now.In(pacific).Month(), now.In(pacific).Day()-trafficDays+1, 0, 0, 0, 0, pacific)
		hits, err := t.hits(r.Context(), from)
		if err != nil {
			slog.Error("traffic", "err", err)
			http.Error(w, "traffic unavailable: "+err.Error(), http.StatusBadGateway)
			return
		}
		var buf bytes.Buffer
		if err := trafficTmpl.Execute(&buf, buildReport(hits, t.host, from, now, t.home(r.Context()))); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		t.cache, t.at = buf.Bytes(), now
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "private, no-store")
	w.Write(t.cache)
}

var trafficTmpl = template.Must(template.New("traffic").Funcs(template.FuncMap{
	"pct": func(n, of int) int {
		if of == 0 {
			return 0
		}
		return n * 100 / of
	},
}).Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="robots" content="noindex, nofollow"><title>Traffic</title>
<style>
body{font:15px/1.4 system-ui,sans-serif;max-width:52rem;margin:2rem auto;padding:0 1rem;color:#222;background:#fff}
@media (prefers-color-scheme:dark){body{color:#ddd;background:#161616}td,th{border-color:#333!important}.bar{background:#c56b8a!important}}
h1{font-size:1.4rem;margin:0}h2{font-size:1.05rem;margin:2rem 0 .5rem}p.sub{color:#888;margin:.25rem 0 1.5rem}
.tot{display:flex;gap:2rem;flex-wrap:wrap}.tot b{display:block;font-size:1.8rem}
table{border-collapse:collapse;width:100%}td,th{padding:.3rem .5rem;border-bottom:1px solid #eee;text-align:left}
td.n,th.n{text-align:right;font-variant-numeric:tabular-nums;width:5rem}td.k{word-break:break-all}
.bar{height:.7rem;background:#e89ab4;border-radius:2px;min-width:1px}
</style></head><body>
<h1>Traffic</h1>
<p class="sub">{{.From.Format "Jan 2"}} – {{.To.Format "Jan 2, 3:04 PM MST"}} · browsers only; bots, scrapers and Nick's home are left out ({{.Dropped}} requests) · cached for an hour, <a href="?refresh">refresh</a></p>
<div class="tot"><div><b>{{.Visitors}}</b>visitors</div><div><b>{{.Views}}</b>page views</div></div>
<h2>By day</h2>
<table><tr><th>Day</th><th class="n">Visitors</th><th class="n">Views</th><th></th></tr>
{{range .Days}}<tr><td>{{.Key}}</td><td class="n">{{.Visitors}}</td><td class="n">{{.Views}}</td><td style="width:40%"><div class="bar" style="width:{{pct .Views $.Max}}%"></div></td></tr>
{{end}}</table>
<h2>Pages</h2>
<table><tr><th>Path</th><th class="n">Visitors</th><th class="n">Views</th></tr>
{{range .Pages}}<tr><td class="k">{{.Key}}</td><td class="n">{{.Visitors}}</td><td class="n">{{.Views}}</td></tr>
{{else}}<tr><td colspan="3">No page views yet.</td></tr>{{end}}</table>
<h2>Referrers</h2>
<table><tr><th>Site</th><th class="n">Visitors</th><th class="n">Views</th></tr>
{{range .Referrers}}<tr><td class="k">{{.Key}}</td><td class="n">{{.Visitors}}</td><td class="n">{{.Views}}</td></tr>
{{else}}<tr><td colspan="3">None.</td></tr>{{end}}</table>
</body></html>
`))
