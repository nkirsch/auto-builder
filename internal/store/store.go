// Package store keeps attachments and run logs in GCS or a local directory.
package store

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

// ErrNotFound is returned by Get for a missing object.
var ErrNotFound = errors.New("object not found")

// Storage puts and gets named objects.
type Storage interface {
	Put(ctx context.Context, name, contentType string, r io.Reader) error
	Get(ctx context.Context, name string) (io.ReadCloser, error)
}

// Dir stores objects as files under a directory (local dev).
type Dir string

func (d Dir) path(name string) (string, error) {
	clean := filepath.Clean("/" + name)
	if clean == "/" {
		return "", fmt.Errorf("bad object name %q", name)
	}
	return filepath.Join(string(d), clean), nil
}

func (d Dir) Put(_ context.Context, name, _ string, r io.Reader) error {
	p, err := d.path(name)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	f, err := os.Create(p)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, r); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func (d Dir) Get(_ context.Context, name string) (io.ReadCloser, error) {
	p, err := d.path(name)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	return f, err
}

// GCS stores objects in a bucket via the JSON API.
type GCS struct {
	Bucket string
	client *http.Client
}

// NewGCS uses Application Default Credentials (the Cloud Run service account).
func NewGCS(ctx context.Context, bucket string) (*GCS, error) {
	ts, err := google.DefaultTokenSource(ctx, "https://www.googleapis.com/auth/devstorage.read_write")
	if err != nil {
		return nil, err
	}
	return &GCS{Bucket: bucket, client: oauth2.NewClient(ctx, ts)}, nil
}

func (g *GCS) Put(ctx context.Context, name, contentType string, r io.Reader) error {
	u := "https://storage.googleapis.com/upload/storage/v1/b/" + url.PathEscape(g.Bucket) +
		"/o?uploadType=media&name=" + url.QueryEscape(name)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, r)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", contentType)
	resp, err := g.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("gcs put %s: %s %s", name, resp.Status, strings.TrimSpace(string(b)))
	}
	return nil
}

func (g *GCS) Get(ctx context.Context, name string) (io.ReadCloser, error) {
	u := "https://storage.googleapis.com/storage/v1/b/" + url.PathEscape(g.Bucket) +
		"/o/" + url.PathEscape(name) + "?alt=media"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := g.client.Do(req)
	if err != nil {
		return nil, err
	}
	switch resp.StatusCode {
	case http.StatusOK:
		return resp.Body, nil
	case http.StatusNotFound:
		resp.Body.Close()
		return nil, ErrNotFound
	default:
		resp.Body.Close()
		return nil, fmt.Errorf("gcs get %s: %s", name, resp.Status)
	}
}
