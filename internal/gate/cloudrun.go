package gate

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

// ServiceSHA reads GIT_SHA from a Cloud Run service's latest ready revision:
// what a tenant's app is serving.
type ServiceSHA struct {
	API     string // "https://run.googleapis.com", overridable in tests
	Service string // projects/P/locations/R/services/S
	client  *http.Client
}

func NewServiceSHA(ctx context.Context, project, region, service string) (*ServiceSHA, error) {
	ts, err := google.DefaultTokenSource(ctx, "https://www.googleapis.com/auth/cloud-platform")
	if err != nil {
		return nil, err
	}
	return &ServiceSHA{API: "https://run.googleapis.com",
		Service: fmt.Sprintf("projects/%s/locations/%s/services/%s", project, region, service),
		client:  oauth2.NewClient(ctx, ts)}, nil
}

// SHA returns the serving revision's GIT_SHA, or "" before the first deploy.
func (s *ServiceSHA) SHA(ctx context.Context) (string, error) {
	var svc struct {
		LatestReadyRevision string `json:"latestReadyRevision"`
	}
	if err := s.get(ctx, s.Service, &svc); err != nil || svc.LatestReadyRevision == "" {
		return "", err
	}
	var rev struct {
		Containers []struct {
			Env []struct{ Name, Value string } `json:"env"`
		} `json:"containers"`
	}
	if err := s.get(ctx, svc.LatestReadyRevision, &rev); err != nil {
		return "", err
	}
	for _, c := range rev.Containers {
		for _, e := range c.Env {
			if e.Name == "GIT_SHA" {
				return e.Value, nil
			}
		}
	}
	return "", nil
}

func (s *ServiceSHA) get(ctx context.Context, name string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.API+"/v2/"+name, nil)
	if err != nil {
		return err
	}
	client := s.client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", name, resp.Status)
	}
	return json.Unmarshal(b, out)
}
