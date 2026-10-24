package gate

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

// LogLauncher only logs (LAUNCHER=none, local dev).
type LogLauncher struct{}

func (LogLauncher) Launch(_ context.Context, runID int) (string, error) {
	slog.Info("launcher=none: would start runner", "run", runID)
	return "none", nil
}

// CloudRunJob runs a Cloud Run Job with RUN_ID overridden.
type CloudRunJob struct {
	Project, Region, Job string
	client               *http.Client
}

func NewCloudRunJob(ctx context.Context, project, region, job string) (*CloudRunJob, error) {
	ts, err := google.DefaultTokenSource(ctx, "https://www.googleapis.com/auth/cloud-platform")
	if err != nil {
		return nil, err
	}
	return &CloudRunJob{Project: project, Region: region, Job: job, client: oauth2.NewClient(ctx, ts)}, nil
}

func (c *CloudRunJob) Launch(ctx context.Context, runID int) (string, error) {
	body, _ := json.Marshal(map[string]any{
		"overrides": map[string]any{
			"containerOverrides": []any{map[string]any{
				"env": []any{map[string]string{"name": "RUN_ID", "value": strconv.Itoa(runID)}},
			}},
		},
	})
	u := fmt.Sprintf("https://run.googleapis.com/v2/projects/%s/locations/%s/jobs/%s:run", c.Project, c.Region, c.Job)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("jobs.run: %s %s", resp.Status, strings.TrimSpace(string(b)))
	}
	// The long-running operation's metadata is the Execution.
	var op struct {
		Name     string `json:"name"`
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
	}
	json.Unmarshal(b, &op)
	if op.Metadata.Name != "" {
		return op.Metadata.Name[strings.LastIndex(op.Metadata.Name, "/")+1:], nil
	}
	return op.Name, nil
}
