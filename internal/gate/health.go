package gate

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// HealthSHA reads the sha from an app's GET /health JSON: what an existing
// tenant's app, which deploys itself, is serving.
type HealthSHA struct {
	URL    string // the app's base URL
	Client *http.Client
}

// SHA returns /health's sha, or "" when the app reports none.
func (h HealthSHA) SHA(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(h.URL, "/")+"/health", nil)
	if err != nil {
		return "", err
	}
	client := h.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GET %s/health: %s", h.URL, resp.Status)
	}
	var v struct {
		SHA string `json:"sha"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&v); err != nil {
		return "", fmt.Errorf("GET %s/health: %w", h.URL, err)
	}
	return v.SHA, nil
}
