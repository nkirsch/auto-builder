package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/nkirsch/auto-builder/internal/github"
	"github.com/nkirsch/auto-builder/internal/tenant"
)

// Shell runs a command (gcloud, git) and returns its stdout.
type Shell interface {
	Run(ctx context.Context, env []string, args ...string) (string, error)
}

// Provisioner creates one app's resources (TENANCY.md "Per-app resources").
// Every step checks first, so a re-run (ACTION=sync) only fills gaps.
type Provisioner struct {
	Project, Region string
	Domain, Zone    string // apps on <slug>.<Domain>; CNAMEs in Cloud DNS zone Zone
	Pool, Provider  string // Workload Identity pool and provider the app repos deploy through
	Template        string // owner/name of the template repo
	GitHub          *github.Client
	Token           func(ctx context.Context) (string, error) // installation token for git
	Shell           Shell
	// Report, when set, records a step's state (running, done, failed).
	Report func(ctx context.Context, step, state, errText string)
}

func (p *Provisioner) gcloud(ctx context.Context, args ...string) (string, error) {
	return p.Shell.Run(ctx, nil, append([]string{"gcloud", "--project", p.Project, "--quiet"}, args...)...)
}

// ensure runs create unless describe succeeds.
func (p *Provisioner) ensure(ctx context.Context, describe, create []string) error {
	if _, err := p.gcloud(ctx, describe...); err == nil {
		return nil
	}
	_, err := p.gcloud(ctx, create...)
	return err
}

func (p *Provisioner) sa(name string) string {
	return name + "@" + p.Project + ".iam.gserviceaccount.com"
}

// Create provisions t: repo, accounts, service, deploy access, domain,
// repo variables, then the template's code (whose push deploys it).
// Each of tenant.ProvisionSteps it covers is reported to Report.
func (p *Provisioner) Create(ctx context.Context, t tenant.Tenant) error {
	svc := "x-" + t.Slug
	// Account IDs must be 6–30 characters; slugs are 2–21.
	runtimeID, deployerID := "app-"+t.Slug, "deploy-"+t.Slug
	runtime, deployer := p.sa(runtimeID), p.sa(deployerID)
	host := t.Slug + "." + p.Domain
	org, name, _ := strings.Cut(t.Repo, "/")

	var empty bool
	if err := p.step(ctx, "Repo", func() error {
		exists, e, err := p.GitHub.RepoState(ctx, t.Repo)
		if err != nil {
			return fmt.Errorf("repo: %w", err)
		}
		empty = e
		if !exists {
			if err := p.GitHub.CreateOrgRepo(ctx, org, name, t.Name+" on "+host); err != nil {
				return fmt.Errorf("create repo: %w", err)
			}
			empty = true
		}
		return nil
	}); err != nil {
		return err
	}

	groups := []struct {
		step  string
		items []struct{ describe, create []string }
	}{
		{"Accounts", []struct{ describe, create []string }{
			{[]string{"iam", "service-accounts", "describe", runtime},
				[]string{"iam", "service-accounts", "create", runtimeID, "--display-name", t.Slug + " app"}},
			{[]string{"iam", "service-accounts", "describe", deployer},
				[]string{"iam", "service-accounts", "create", deployerID, "--display-name", t.Slug + " deploys"}},
		}},
		{"Image repo and service", []struct{ describe, create []string }{
			{[]string{"artifacts", "repositories", "describe", svc, "--location", p.Region},
				[]string{"artifacts", "repositories", "create", svc, "--location", p.Region, "--repository-format", "docker"}},
			{[]string{"run", "services", "describe", svc, "--region", p.Region},
				[]string{"run", "deploy", svc, "--region", p.Region, "--image", "us-docker.pkg.dev/cloudrun/container/hello",
					"--service-account", runtime, "--allow-unauthenticated"}},
		}},
		{"Domain mapping", []struct{ describe, create []string }{
			{[]string{"beta", "run", "domain-mappings", "describe", "--domain", host, "--region", p.Region},
				[]string{"beta", "run", "domain-mappings", "create", "--service", svc, "--domain", host, "--region", p.Region}},
		}},
		// The board's /logs reads the app's request log through this view.
		{"Request log view", []struct{ describe, create []string }{
			{[]string{"logging", "views", "describe", t.Slug + "-requests", "--bucket", "_Default", "--location", "global"},
				[]string{"logging", "views", "create", t.Slug + "-requests", "--bucket", "_Default", "--location", "global",
					"--log-filter", fmt.Sprintf(`resource.type="cloud_run_revision" AND resource.labels.service_name=%q AND LOG_ID("run.googleapis.com/requests")`, svc)}},
		}},
	}
	for _, g := range groups {
		if err := p.step(ctx, g.step, func() error {
			for _, s := range g.items {
				if err := p.ensure(ctx, s.describe, s.create); err != nil {
					return fmt.Errorf("%s: %w", strings.Join(s.create[:2], " "), err)
				}
			}
			return nil
		}); err != nil {
			return err
		}
	}
	if err := p.step(ctx, "DNS record", func() error {
		if err := p.cname(ctx, host); err != nil {
			return fmt.Errorf("dns: %w", err)
		}
		return nil
	}); err != nil {
		return err
	}
	if err := p.step(ctx, "Deploy access", func() error {
		return p.deployAccess(ctx, t.Repo, svc, runtime, deployer)
	}); err != nil {
		return err
	}
	return p.step(ctx, "App code", func() error {
		if !empty {
			return nil
		}
		if err := p.pushTemplate(ctx, t.Repo); err != nil {
			return fmt.Errorf("template: %w", err)
		}
		return nil
	})
}

// deployAccess lets repo's master branch deploy the app as deployer, and
// sets the repo variables its deploy workflow reads.
func (p *Provisioner) deployAccess(ctx context.Context, repo, svc, runtime, deployer string) error {
	number, err := p.gcloud(ctx, "projects", "describe", p.Project, "--format=value(projectNumber)")
	if err != nil {
		return err
	}
	number = strings.TrimSpace(number)
	// The deployer reaches only this app: its service, its image repo, acting
	// as its runtime account, and only from this repo's master branch.
	repoPrincipal := fmt.Sprintf("principalSet://iam.googleapis.com/projects/%s/locations/global/workloadIdentityPools/%s/attribute.repository/%s",
		number, p.Pool, repo)
	bindings := [][]string{
		{"run", "services", "add-iam-policy-binding", svc, "--region", p.Region,
			"--member", "serviceAccount:" + deployer, "--role", "roles/run.developer"},
		{"artifacts", "repositories", "add-iam-policy-binding", svc, "--location", p.Region,
			"--member", "serviceAccount:" + deployer, "--role", "roles/artifactregistry.writer"},
		{"iam", "service-accounts", "add-iam-policy-binding", runtime,
			"--member", "serviceAccount:" + deployer, "--role", "roles/iam.serviceAccountUser"},
		{"iam", "service-accounts", "add-iam-policy-binding", deployer,
			"--member", repoPrincipal, "--role", "roles/iam.workloadIdentityUser"},
	}
	for _, b := range bindings {
		if _, err := p.gcloud(ctx, append(b, "--format=none")...); err != nil {
			return fmt.Errorf("%s %s: %w", b[0], b[3], err)
		}
	}

	vars := map[string]string{
		"GCP_PROJECT": p.Project, "GCP_REGION": p.Region, "SERVICE": svc, "GCP_DEPLOYER": deployer,
		"IMAGE": fmt.Sprintf("%s-docker.pkg.dev/%s/%s/app", p.Region, p.Project, svc),
		"GCP_WIF_PROVIDER": fmt.Sprintf("projects/%s/locations/global/workloadIdentityPools/%s/providers/%s",
			number, p.Pool, p.Provider),
	}
	for k, v := range vars {
		if err := p.GitHub.SetVariable(ctx, repo, k, v); err != nil {
			return fmt.Errorf("variable %s: %w", k, err)
		}
	}
	return nil
}

// step reports name running, runs f, then reports it done or failed.
func (p *Provisioner) step(ctx context.Context, name string, f func() error) error {
	p.report(ctx, name, "running", "")
	if err := f(); err != nil {
		p.report(ctx, name, "failed", err.Error())
		return err
	}
	p.report(ctx, name, "done", "")
	return nil
}

func (p *Provisioner) report(ctx context.Context, name, state, errText string) {
	if p.Report != nil {
		p.Report(context.WithoutCancel(ctx), name, state, errText)
	}
}

// Delete tears down t's Google Cloud resources and archives its repo, so
// its code and history stay readable. Missing pieces are skipped.
func (p *Provisioner) Delete(ctx context.Context, t tenant.Tenant) error {
	svc := "x-" + t.Slug
	host := t.Slug + "." + p.Domain
	steps := [][]string{
		{"beta", "run", "domain-mappings", "delete", "--domain", host, "--region", p.Region},
		{"dns", "record-sets", "delete", host + ".", "--type", "CNAME", "--zone", p.Zone},
		{"run", "services", "delete", svc, "--region", p.Region},
		{"artifacts", "repositories", "delete", svc, "--location", p.Region},
		{"logging", "views", "delete", t.Slug + "-requests", "--bucket", "_Default", "--location", "global"},
	}
	// Deleting a missing account reads as PERMISSION_DENIED, so list first.
	for _, sa := range []string{p.sa("deploy-" + t.Slug), p.sa("app-" + t.Slug)} {
		got, err := p.gcloud(ctx, "iam", "service-accounts", "list", "--filter", "email="+sa, "--format=value(email)")
		if err != nil || strings.TrimSpace(got) != "" {
			steps = append(steps, []string{"iam", "service-accounts", "delete", sa})
		}
	}
	var errs []error
	for _, s := range steps {
		if _, err := p.gcloud(ctx, s...); err != nil && !notFound(err) {
			errs = append(errs, fmt.Errorf("%s %s: %w", s[0], s[1], err))
		}
	}
	if err := p.GitHub.Archive(ctx, t.Repo); err != nil && !github.IsNotFound(err) {
		errs = append(errs, fmt.Errorf("archive repo: %w", err))
	}
	return errors.Join(errs...)
}

// notFound is a gcloud error for something already gone.
func notFound(err error) bool {
	m := err.Error()
	return strings.Contains(m, "NOT_FOUND") || strings.Contains(m, "not found") || strings.Contains(m, "could not be found") ||
		strings.Contains(m, "does not exist")
}

// cname points host at Cloud Run's domain-mapping front end, replacing a
// record that points elsewhere (e.g. www's old static site).
func (p *Provisioner) cname(ctx context.Context, host string) error {
	const target = "ghs.googlehosted.com."
	// gcloud takes the zone after the verb, not after "record-sets".
	rec := func(verb string) []string {
		return []string{"dns", "record-sets", verb, host + ".", "--type", "CNAME", "--ttl", "300",
			"--rrdatas", target, "--zone", p.Zone}
	}
	got, err := p.gcloud(ctx, "dns", "record-sets", "describe", host+".", "--type", "CNAME",
		"--zone", p.Zone, "--format=value(rrdatas)")
	switch {
	case err != nil:
		_, err = p.gcloud(ctx, rec("create")...)
	case strings.TrimSpace(got) != target:
		_, err = p.gcloud(ctx, rec("update")...)
	}
	return err
}

// pushTemplate copies the template's master into the empty repo; the push
// runs the app's first deploy, now that its variables are set.
func (p *Provisioner) pushTemplate(ctx context.Context, repo string) error {
	token, err := p.Token(ctx)
	if err != nil {
		return err
	}
	dir, err := os.MkdirTemp("", "template")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	// The token goes in an env-only header, never argv or .git/config.
	auth := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token))
	env := append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=http.https://github.com/.extraheader", "GIT_CONFIG_VALUE_0=AUTHORIZATION: basic "+auth)
	for _, args := range [][]string{
		{"git", "clone", "--quiet", "--branch", "master", "https://github.com/" + p.Template + ".git", dir},
		{"git", "-C", dir, "push", "--quiet", "https://github.com/" + repo + ".git", "HEAD:refs/heads/master"},
	} {
		if _, err := p.Shell.Run(ctx, env, args...); err != nil {
			return err
		}
	}
	return nil
}

// errUsage is a bad TENANT or ACTION.
var errUsage = errors.New("usage")
