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
func (p *Provisioner) Create(ctx context.Context, t tenant.Tenant) error {
	svc := "x-" + t.Slug
	runtime, deployer := p.sa(svc), p.sa(svc+"-deploy")
	host := t.Slug + "." + p.Domain
	org, name, _ := strings.Cut(t.Repo, "/")

	exists, empty, err := p.GitHub.RepoState(ctx, t.Repo)
	if err != nil {
		return fmt.Errorf("repo: %w", err)
	}
	if !exists {
		if err := p.GitHub.CreateOrgRepo(ctx, org, name, t.Name+" on "+host); err != nil {
			return fmt.Errorf("create repo: %w", err)
		}
		empty = true
	}

	number, err := p.gcloud(ctx, "projects", "describe", p.Project, "--format=value(projectNumber)")
	if err != nil {
		return err
	}
	number = strings.TrimSpace(number)
	steps := []struct{ describe, create []string }{
		{[]string{"iam", "service-accounts", "describe", runtime},
			[]string{"iam", "service-accounts", "create", svc, "--display-name", t.Slug + " app"}},
		{[]string{"iam", "service-accounts", "describe", deployer},
			[]string{"iam", "service-accounts", "create", svc + "-deploy", "--display-name", t.Slug + " deploys"}},
		{[]string{"artifacts", "repositories", "describe", svc, "--location", p.Region},
			[]string{"artifacts", "repositories", "create", svc, "--location", p.Region, "--repository-format", "docker"}},
		{[]string{"run", "services", "describe", svc, "--region", p.Region},
			[]string{"run", "deploy", svc, "--region", p.Region, "--image", "us-docker.pkg.dev/cloudrun/container/hello",
				"--service-account", runtime, "--allow-unauthenticated"}},
		{[]string{"beta", "run", "domain-mappings", "describe", "--domain", host, "--region", p.Region},
			[]string{"beta", "run", "domain-mappings", "create", "--service", svc, "--domain", host, "--region", p.Region}},
		{[]string{"dns", "record-sets", "describe", host + ".", "--type", "CNAME", "--zone", p.Zone},
			[]string{"dns", "record-sets", "create", host + ".", "--type", "CNAME", "--ttl", "300",
				"--rrdatas", "ghs.googlehosted.com.", "--zone", p.Zone}},
	}
	for _, s := range steps {
		if err := p.ensure(ctx, s.describe, s.create); err != nil {
			return fmt.Errorf("%s: %w", strings.Join(s.create[:2], " "), err)
		}
	}
	// The deployer reaches only this app: its service, its image repo, acting
	// as its runtime account, and only from this repo's master branch.
	repoPrincipal := fmt.Sprintf("principalSet://iam.googleapis.com/projects/%s/locations/global/workloadIdentityPools/%s/attribute.repository/%s",
		number, p.Pool, t.Repo)
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
		if err := p.GitHub.SetVariable(ctx, t.Repo, k, v); err != nil {
			return fmt.Errorf("variable %s: %w", k, err)
		}
	}
	if empty {
		if err := p.pushTemplate(ctx, t.Repo); err != nil {
			return fmt.Errorf("template: %w", err)
		}
	}
	return nil
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
