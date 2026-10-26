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

func (LogLauncher) LaunchEnv(_ context.Context, env map[string]string) (string, error) {
	slog.Info("launcher=none: would start runner", "run", env["RUN_ID"], "tenant", env["TENANT"])
	return "none", nil
}

// EnvLauncher starts the runner job with env overrides (multi-tenant mode).
type EnvLauncher interface {
	LaunchEnv(ctx context.Context, env map[string]string) (string, error)
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
	return c.LaunchEnv(ctx, map[string]string{"RUN_ID": strconv.Itoa(runID)})
}

// Cancel stops one execution of the job.
func (c *CloudRunJob) Cancel(ctx context.Context, _ int, execution string) error {
	u := fmt.Sprintf("https://run.googleapis.com/v2/projects/%s/locations/%s/jobs/%s/executions/%s:cancel",
		c.Project, c.Region, c.Job, execution)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, strings.NewReader("{}"))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<10))
		return fmt.Errorf("executions.cancel: %s %s", resp.Status, strings.TrimSpace(string(b)))
	}
	return nil
}

// LaunchEnv runs the job with env overridden, returning the execution name.
func (c *CloudRunJob) LaunchEnv(ctx context.Context, env map[string]string) (string, error) {
	var vars []any
	for k, v := range env {
		vars = append(vars, map[string]string{"name": k, "value": v})
	}
	body, _ := json.Marshal(map[string]any{
		"overrides": map[string]any{
			"containerOverrides": []any{map[string]any{"env": vars}},
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
