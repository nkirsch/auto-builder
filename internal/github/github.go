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
	j, err := a.JWT()
	if err != nil {
		return "", time.Time{}, err
	}
	var out struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	c := &Client{API: a.API, http: a.client, bearer: j}
	if err := c.do(ctx, http.MethodPost, "/app/installations/"+a.InstallationID+"/access_tokens", nil, &out); err != nil {
		return "", time.Time{}, fmt.Errorf("installation token: %w", err)
	}
	return out.Token, out.ExpiresAt, nil
}

// BotIdentity returns the app's commit identity: "<slug>[bot]" and its noreply email.
func (a *App) BotIdentity(ctx context.Context) (name, email string, err error) {
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
	if err := New(a.API, "").do(ctx, http.MethodGet, "/users/"+name, nil, &user); err != nil {
		return "", "", err
	}
	return name, fmt.Sprintf("%d+%s@users.noreply.github.com", user.ID, name), nil
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
