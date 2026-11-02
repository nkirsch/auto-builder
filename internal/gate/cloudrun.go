package gate

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

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

// Revision is one of a service's Cloud Run revisions and the GIT_SHA it runs.
type Revision struct {
	Name    string // short name, as request logs label it
	Created time.Time
	SHA     string
}

// Revisions lists the service's revisions, at most pages of 100.
func (s *ServiceSHA) Revisions(ctx context.Context, pages int) ([]Revision, error) {
	var out []Revision
	token := ""
	for range pages {
		var list struct {
			Revisions []struct {
				Name       string    `json:"name"`
				CreateTime time.Time `json:"createTime"`
				Containers []struct {
					Env []struct{ Name, Value string } `json:"env"`
				} `json:"containers"`
			} `json:"revisions"`
			NextPageToken string `json:"nextPageToken"`
		}
		path := s.Service + "/revisions?pageSize=100"
		if token != "" {
			path += "&pageToken=" + url.QueryEscape(token)
		}
		if err := s.get(ctx, path, &list); err != nil {
			return nil, err
		}
		for _, r := range list.Revisions {
			rev := Revision{Name: r.Name[strings.LastIndex(r.Name, "/")+1:], Created: r.CreateTime}
			for _, c := range r.Containers {
				for _, e := range c.Env {
					if e.Name == "GIT_SHA" {
						rev.SHA = e.Value
					}
				}
			}
			out = append(out, rev)
		}
		if token = list.NextPageToken; token == "" {
			break
		}
	}
	return out, nil
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
