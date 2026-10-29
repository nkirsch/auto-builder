// Package github is a small REST client: GitHub App installation tokens, PR
// state and commit comparison.
package github

import (
	"bytes"
	"context"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const api = "https://api.github.com"

// App mints installation tokens for a GitHub App.
type App struct {
	ID             string
	InstallationID string
	key            *rsa.PrivateKey
	API            string
	client         *http.Client

	mu      sync.Mutex
	token   string
	expires time.Time

	idMu     sync.Mutex
	botName  string
	botEmail string
}

// NewApp parses a PEM private key. Returns nil, nil when any part is unset.
func NewApp(id, installation, pemKey string) (*App, error) {
	if id == "" || installation == "" || pemKey == "" {
		return nil, nil
	}
	key, err := jwt.ParseRSAPrivateKeyFromPEM([]byte(pemKey))
	if err != nil {
		return nil, fmt.Errorf("GH_APP_PRIVATE_KEY: %w", err)
	}
	return &App{ID: id, InstallationID: installation, key: key, API: api,
		client: &http.Client{Timeout: 30 * time.Second}}, nil
}

// JWT returns an app JWT valid for 9 minutes (GitHub's max is 10).
func (a *App) JWT() (string, error) {
	now := time.Now()
	return jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.RegisteredClaims{
		IssuedAt:  jwt.NewNumericDate(now.Add(-time.Minute)),
		ExpiresAt: jwt.NewNumericDate(now.Add(9 * time.Minute)),
		Issuer:    a.ID,
	}).SignedString(a.key)
}

// Token returns a cached installation token, refreshed 5 minutes before expiry.
func (a *App) Token(ctx context.Context) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.token != "" && time.Until(a.expires) > 5*time.Minute {
		return a.token, nil
	}
	token, expires, err := a.Mint(ctx)
	if err != nil {
		return "", err
	}
	a.token, a.expires = token, expires
	return a.token, nil
}

// Mint returns a new, uncached installation token (GitHub gives it 1 h).
func (a *App) Mint(ctx context.Context) (token string, expires time.Time, err error) {
	return a.MintFor(ctx, "", nil)
}

// AgentPermissions is what a run's agent gets: push branches, open and merge
// PRs, read CI, and with workflows, change what CI and deploys run.
func AgentPermissions(workflows bool) map[string]string {
	p := map[string]string{
		"contents": "write", "pull_requests": "write", "checks": "read", "actions": "read", "metadata": "read",
	}
	if workflows {
		p["workflows"] = "write"
	}
	return p
}

// UpstreamPermissions is what an upstream merge run gets: it pushes
// upstream's commits, workflows included, to a branch.
var UpstreamPermissions = map[string]string{"contents": "write", "workflows": "write", "metadata": "read"}

// MintFor is Mint limited to one repo ("owner/name") of the installation and
// to perms; "" means every repo it covers, nil every permission the App has.
func (a *App) MintFor(ctx context.Context, repo string, perms map[string]string) (token string, expires time.Time, err error) {
	j, err := a.JWT()
	if err != nil {
		return "", time.Time{}, err
	}
	var out struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	scope := map[string]any{}
	if repo != "" {
		scope["repositories"] = []string{repo[strings.Index(repo, "/")+1:]}
	}
	if perms != nil {
		scope["permissions"] = perms
	}
	var in any
	if len(scope) > 0 {
		in = scope
	}
	c := &Client{API: a.API, http: a.client, bearer: j}
	if err := c.do(ctx, http.MethodPost, "/app/installations/"+a.InstallationID+"/access_tokens", in, &out); err != nil {
		return "", time.Time{}, fmt.Errorf("installation token: %w", err)
	}
	return out.Token, out.ExpiresAt, nil
}

// BotIdentity returns the app's commit identity: "<slug>[bot]" and its noreply
// email. Looked up once, authenticated: anonymous calls share Cloud Run's
// egress IP rate limit.
func (a *App) BotIdentity(ctx context.Context) (name, email string, err error) {
	a.idMu.Lock()
	defer a.idMu.Unlock()
	if a.botName != "" {
		return a.botName, a.botEmail, nil
	}
	j, err := a.JWT()
	if err != nil {
		return "", "", err
	}
	var app struct {
		Slug string `json:"slug"`
	}
	if err := (&Client{API: a.API, http: a.client, bearer: j}).do(ctx, http.MethodGet, "/app", nil, &app); err != nil {
		return "", "", err
	}
	name = app.Slug + "[bot]"
	var user struct {
		ID int64 `json:"id"`
	}
	if err := ForApp(a).do(ctx, http.MethodGet, "/users/"+name, nil, &user); err != nil {
		return "", "", err
	}
	a.botName, a.botEmail = name, fmt.Sprintf("%d+%s@users.noreply.github.com", user.ID, name)
	return a.botName, a.botEmail, nil
}

// Client calls the REST API with an optional token source.
type Client struct {
	API    string
	http   *http.Client
	bearer string
	app    *App
}

// New returns a client using a fixed token (empty for anonymous).
func New(apiBase, token string) *Client {
	if apiBase == "" {
		apiBase = api
	}
	return &Client{API: apiBase, http: &http.Client{Timeout: 30 * time.Second}, bearer: token}
}

// ForApp returns a client authenticated as the app installation.
func ForApp(a *App) *Client {
	return &Client{API: a.API, http: a.client, app: a}
}

// StatusError is a non-2xx API response.
type StatusError struct {
	Code int
	Body string
}

func (e *StatusError) Error() string { return fmt.Sprintf("github: %d %s", e.Code, e.Body) }

// IsNotFound reports a 404.
func IsNotFound(err error) bool {
	var se *StatusError
	return errors.As(err, &se) && se.Code == http.StatusNotFound
}

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.API+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	token := c.bearer
	if c.app != nil {
		if token, err = c.app.Token(ctx); err != nil {
			return err
		}
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return &StatusError{Code: resp.StatusCode, Body: strings.TrimSpace(string(b))}
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// PR is the subset of a pull request the ship-checker needs.
type PR struct {
	Number         int    `json:"number"`
	State          string `json:"state"` // open, closed
	Merged         bool   `json:"merged"`
	MergeCommitSHA string `json:"merge_commit_sha"`
	HTMLURL        string `json:"html_url"`
	Head           struct {
		SHA string `json:"sha"`
	} `json:"head"`
}

func (c *Client) PR(ctx context.Context, repo string, n int) (PR, error) {
	var pr PR
	err := c.do(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/pulls/%d", repo, n), nil, &pr)
	return pr, err
}

// ChecksFailed reports whether any check run on commit sha failed or timed out.
func (c *Client) ChecksFailed(ctx context.Context, repo, sha string) (bool, error) {
	var out struct {
		CheckRuns []struct {
			Conclusion string `json:"conclusion"`
		} `json:"check_runs"`
	}
	if err := c.do(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/commits/%s/check-runs?per_page=100", repo, sha), nil, &out); err != nil {
		return false, err
	}
	for _, r := range out.CheckRuns {
		if r.Conclusion == "failure" || r.Conclusion == "timed_out" {
			return true, nil
		}
	}
	return false, nil
}

// Contains reports whether commit sha is reachable from head.
func (c *Client) Contains(ctx context.Context, repo, head, sha string) (bool, error) {
	var cmp struct {
		Status string `json:"status"` // identical, ahead, behind, diverged
	}
	if err := c.do(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/compare/%s...%s", repo, head, sha), nil, &cmp); err != nil {
		return false, err
	}
	return cmp.Status == "identical" || cmp.Status == "behind", nil
}

// Version names commit sha "vYY.MM.DD-N": the UTC day it was committed and its
// position among that day's commits reachable from it.
func (c *Client) Version(ctx context.Context, repo, sha string) (string, error) {
	var commit struct {
		Commit struct {
			Committer struct {
				Date time.Time `json:"date"`
			} `json:"committer"`
		} `json:"commit"`
	}
	if err := c.do(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/commits/%s", repo, sha), nil, &commit); err != nil {
		return "", err
	}
	at := commit.Commit.Committer.Date.UTC()
	day := time.Date(at.Year(), at.Month(), at.Day(), 0, 0, 0, 0, time.UTC)
	var list []struct {
		SHA string `json:"sha"`
	}
	q := url.Values{"sha": {sha}, "since": {day.Format(time.RFC3339)}, "per_page": {"100"}}
	if err := c.do(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/commits?%s", repo, q.Encode()), nil, &list); err != nil {
		return "", err
	}
	return fmt.Sprintf("v%s-%d", at.Format("06.01.02"), max(len(list), 1)), nil
}

// Tag creates lightweight tag name on commit sha. A tag that already points
// at sha is fine; one pointing elsewhere is an error.
func (c *Client) Tag(ctx context.Context, repo, name, sha string) error {
	err := c.do(ctx, http.MethodPost, fmt.Sprintf("/repos/%s/git/refs", repo),
		map[string]string{"ref": "refs/tags/" + name, "sha": sha}, nil)
	var se *StatusError
	if !errors.As(err, &se) || se.Code != http.StatusUnprocessableEntity {
		return err
	}
	var ref struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	if err := c.do(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/git/ref/tags/%s", repo, url.PathEscape(name)), nil, &ref); err != nil {
		return err
	}
	if ref.Object.SHA != sha {
		return fmt.Errorf("tag %s already points at %s", name, ref.Object.SHA)
	}
	return nil
}

// Issue is the subset of an issue the issue sync needs. The issues API also
// returns pull requests; those have PullRequest set.
type Issue struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
	Body   string `json:"body"`
	User   struct {
		Login string `json:"login"`
	} `json:"user"`
	// AuthorAssociation is OWNER, MEMBER or COLLABORATOR when the author can write to the repo.
	AuthorAssociation string    `json:"author_association"`
	PullRequest       *struct{} `json:"pull_request"`
}

// IssueComment is a comment on an issue (or pull request).
type IssueComment struct {
	ID   int64  `json:"id"`
	Body string `json:"body"`
	User struct {
		Login string `json:"login"`
	} `json:"user"`
	IssueURL string `json:"issue_url"`
}

// IssueNumber is the number at the end of IssueURL, 0 if there is none.
func (c IssueComment) IssueNumber() int {
	n, _ := strconv.Atoi(c.IssueURL[strings.LastIndex(c.IssueURL, "/")+1:])
	return n
}

// maxPages caps paging through issues and comments (100 per page).
const maxPages = 10

// OpenIssues lists open issues and pull requests updated at or after since
// (all when zero), least recently updated first.
func (c *Client) OpenIssues(ctx context.Context, repo string, since time.Time) ([]Issue, error) {
	var all []Issue
	err := c.pages(ctx, fmt.Sprintf("/repos/%s/issues", repo), url.Values{"state": {"open"}}, since, func(page []byte) (int, error) {
		var list []Issue
		err := json.Unmarshal(page, &list)
		all = append(all, list...)
		return len(list), err
	})
	return all, err
}

// IssueComments lists comments on the repo's issues and pull requests updated
// at or after since (all when zero), least recently updated first.
func (c *Client) IssueComments(ctx context.Context, repo string, since time.Time) ([]IssueComment, error) {
	var all []IssueComment
	err := c.pages(ctx, fmt.Sprintf("/repos/%s/issues/comments", repo), url.Values{}, since, func(page []byte) (int, error) {
		var list []IssueComment
		err := json.Unmarshal(page, &list)
		all = append(all, list...)
		return len(list), err
	})
	return all, err
}

// pages GETs path sorted by update time, passing each page to add until a
// short page or maxPages.
func (c *Client) pages(ctx context.Context, path string, q url.Values, since time.Time, add func([]byte) (int, error)) error {
	q.Set("sort", "updated")
	q.Set("direction", "asc")
	q.Set("per_page", "100")
	if !since.IsZero() {
		q.Set("since", since.UTC().Format(time.RFC3339))
	}
	for page := 1; page <= maxPages; page++ {
		q.Set("page", strconv.Itoa(page))
		var raw json.RawMessage
		if err := c.do(ctx, http.MethodGet, path+"?"+q.Encode(), nil, &raw); err != nil {
			return err
		}
		n, err := add(raw)
		if err != nil || n < 100 {
			return err
		}
	}
	return nil
}

// CreateIssue opens an issue and returns its number.
func (c *Client) CreateIssue(ctx context.Context, repo, title, body string) (int, error) {
	var out struct {
		Number int `json:"number"`
	}
	err := c.do(ctx, http.MethodPost, fmt.Sprintf("/repos/%s/issues", repo),
		map[string]string{"title": title, "body": body}, &out)
	return out.Number, err
}

// Comment posts a comment on issue n and returns its ID.
func (c *Client) Comment(ctx context.Context, repo string, n int, body string) (int64, error) {
	var out struct {
		ID int64 `json:"id"`
	}
	err := c.do(ctx, http.MethodPost, fmt.Sprintf("/repos/%s/issues/%d/comments", repo, n),
		map[string]string{"body": body}, &out)
	return out.ID, err
}

// PinIssue pins issue n to the top of the repo's issues (GraphQL only).
func (c *Client) PinIssue(ctx context.Context, repo string, n int) error {
	var is struct {
		NodeID string `json:"node_id"`
	}
	if err := c.do(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/issues/%d", repo, n), nil, &is); err != nil {
		return err
	}
	var out struct {
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := c.do(ctx, http.MethodPost, "/graphql", map[string]any{
		"query":     `mutation($id: ID!) { pinIssue(input: {issueId: $id}) { issue { number } } }`,
		"variables": map[string]string{"id": is.NodeID},
	}, &out); err != nil {
		return err
	}
	if len(out.Errors) > 0 {
		return fmt.Errorf("github: pin issue %d: %s", n, out.Errors[0].Message)
	}
	return nil
}

// Reassign removes and then adds assignees on issue n; GitHub skips logins
// that can't be assigned on the repo.
func (c *Client) Reassign(ctx context.Context, repo string, n int, remove, add []string) error {
	path := fmt.Sprintf("/repos/%s/issues/%d/assignees", repo, n)
	if len(remove) > 0 {
		if err := c.do(ctx, http.MethodDelete, path, map[string][]string{"assignees": remove}, nil); err != nil {
			return err
		}
	}
	if len(add) == 0 {
		return nil
	}
	return c.do(ctx, http.MethodPost, path, map[string][]string{"assignees": add}, nil)
}

// ReopenIssue reopens issue n.
func (c *Client) ReopenIssue(ctx context.Context, repo string, n int) error {
	return c.do(ctx, http.MethodPatch, fmt.Sprintf("/repos/%s/issues/%d", repo, n),
		map[string]string{"state": "open"}, nil)
}

// CloseIssue comments on issue n, then closes it as completed or not planned.
func (c *Client) CloseIssue(ctx context.Context, repo string, n int, comment string, completed bool) error {
	if err := c.do(ctx, http.MethodPost, fmt.Sprintf("/repos/%s/issues/%d/comments", repo, n),
		map[string]string{"body": comment}, nil); err != nil {
		return err
	}
	reason := "not_planned"
	if completed {
		reason = "completed"
	}
	return c.do(ctx, http.MethodPatch, fmt.Sprintf("/repos/%s/issues/%d", repo, n),
		map[string]string{"state": "closed", "state_reason": reason}, nil)
}

// LatestRelease returns the latest published release's tag, or "" if there is none.
func (c *Client) LatestRelease(ctx context.Context, repo string) (string, error) {
	var rel struct {
		Tag string `json:"tag_name"`
	}
	if err := c.do(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/releases/latest", repo), nil, &rel); err != nil {
		if IsNotFound(err) {
			return "", nil
		}
		return "", err
	}
	return rel.Tag, nil
}

// Unreleased returns the default branch's head and how many commits it is
// ahead of tag (all of them when tag is "").
func (c *Client) Unreleased(ctx context.Context, repo, tag string) (head string, ahead int, err error) {
	var commit struct {
		SHA string `json:"sha"`
	}
	if err := c.do(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/commits/HEAD", repo), nil, &commit); err != nil {
		return "", 0, err
	}
	if tag == "" {
		return commit.SHA, 1, nil
	}
	var cmp struct {
		Ahead int `json:"ahead_by"`
	}
	if err := c.do(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/compare/%s...%s", repo, tag, commit.SHA), nil, &cmp); err != nil {
		return "", 0, err
	}
	return commit.SHA, cmp.Ahead, nil
}

// TagsWithPrefix lists tag names starting with prefix.
func (c *Client) TagsWithPrefix(ctx context.Context, repo, prefix string) ([]string, error) {
	var refs []struct {
		Ref string `json:"ref"`
	}
	if err := c.do(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/git/matching-refs/tags/%s", repo, prefix), nil, &refs); err != nil {
		return nil, err
	}
	tags := make([]string, len(refs))
	for i, r := range refs {
		tags[i] = strings.TrimPrefix(r.Ref, "refs/tags/")
	}
	return tags, nil
}

// CreateRelease publishes release tag at sha with generated notes. Made with
// an App token, it triggers the repo's release workflows.
func (c *Client) CreateRelease(ctx context.Context, repo, tag, sha string) (string, error) {
	in := map[string]any{"tag_name": tag, "target_commitish": sha, "name": tag, "generate_release_notes": true}
	var out struct {
		URL string `json:"html_url"`
	}
	if err := c.do(ctx, http.MethodPost, fmt.Sprintf("/repos/%s/releases", repo), in, &out); err != nil {
		return "", err
	}
	return out.URL, nil
}

// RepoState reports whether repo exists and, if so, whether it has no commits.
func (c *Client) RepoState(ctx context.Context, repo string) (exists, empty bool, err error) {
	if err := c.do(ctx, http.MethodGet, "/repos/"+repo, nil, nil); IsNotFound(err) {
		return false, false, nil
	} else if err != nil {
		return false, false, err
	}
	var se *StatusError
	err = c.do(ctx, http.MethodGet, "/repos/"+repo+"/commits?per_page=1", nil, nil)
	if errors.As(err, &se) && se.Code == http.StatusConflict { // "Git Repository is empty."
		return true, true, nil
	}
	return true, false, err
}

// CreateOrgRepo creates an empty private repo in org.
func (c *Client) CreateOrgRepo(ctx context.Context, org, name, description string) error {
	return c.do(ctx, http.MethodPost, "/orgs/"+org+"/repos",
		map[string]any{"name": name, "description": description, "private": true}, nil)
}

// SetVariable creates or updates an Actions variable on repo.
func (c *Client) SetVariable(ctx context.Context, repo, name, value string) error {
	v := map[string]string{"name": name, "value": value}
	err := c.do(ctx, http.MethodPost, "/repos/"+repo+"/actions/variables", v, nil)
	var se *StatusError
	if errors.As(err, &se) && se.Code == http.StatusConflict {
		return c.do(ctx, http.MethodPatch, "/repos/"+repo+"/actions/variables/"+name, v, nil)
	}
	return err
}

// Archive makes repo read-only.
func (c *Client) Archive(ctx context.Context, repo string) error {
	return c.do(ctx, http.MethodPatch, "/repos/"+repo, map[string]bool{"archived": true}, nil)
}

// Head returns the commit SHA at the tip of branch.
func (c *Client) Head(ctx context.Context, repo, branch string) (string, error) {
	var out struct {
		SHA string `json:"sha"`
	}
	err := c.do(ctx, http.MethodGet, "/repos/"+repo+"/commits/"+branch, nil, &out)
	return out.SHA, err
}

// ChecksPassed reports whether sha has check runs and all finished green.
func (c *Client) ChecksPassed(ctx context.Context, repo, sha string) (bool, error) {
	var out struct {
		CheckRuns []struct {
			Status     string `json:"status"`
			Conclusion string `json:"conclusion"`
		} `json:"check_runs"`
	}
	if err := c.do(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/commits/%s/check-runs?per_page=100", repo, sha), nil, &out); err != nil {
		return false, err
	}
	for _, r := range out.CheckRuns {
		if r.Status != "completed" || (r.Conclusion != "success" && r.Conclusion != "neutral" && r.Conclusion != "skipped") {
			return false, nil
		}
	}
	return len(out.CheckRuns) > 0, nil
}

// OpenPR returns the number of the open PR from branch, or 0.
func (c *Client) OpenPR(ctx context.Context, repo, branch string) (int, error) {
	owner, _, _ := strings.Cut(repo, "/")
	var out []struct {
		Number int `json:"number"`
	}
	q := url.Values{"head": {owner + ":" + branch}, "state": {"open"}}
	err := c.do(ctx, http.MethodGet, "/repos/"+repo+"/pulls?"+q.Encode(), nil, &out)
	if err != nil || len(out) == 0 {
		return 0, err
	}
	return out[0].Number, nil
}

// CreatePR opens a PR from head into base and returns its number.
func (c *Client) CreatePR(ctx context.Context, repo, head, base, title, body string) (int, error) {
	var out struct {
		Number int `json:"number"`
	}
	err := c.do(ctx, http.MethodPost, "/repos/"+repo+"/pulls",
		map[string]string{"head": head, "base": base, "title": title, "body": body}, &out)
	return out.Number, err
}

// MergePR merges PR n with a merge commit titled title, keeping the
// branch's history (an upstream merge must not be squashed).
func (c *Client) MergePR(ctx context.Context, repo string, n int, title string) error {
	return c.do(ctx, http.MethodPut, fmt.Sprintf("/repos/%s/pulls/%d/merge", repo, n),
		map[string]string{"merge_method": "merge", "commit_title": title}, nil)
}
