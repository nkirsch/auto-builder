// Command provision is the extreme-provision Cloud Run Job: it creates or
// syncs one tenant's resources (TENANCY.md) and records the outcome in
// control.tenants. Deterministic; no agent. The only inputs are TENANT and
// ACTION; everything else comes from the job's own environment.
package main

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/nkirsch/auto-builder/internal/github"
	"github.com/nkirsch/auto-builder/internal/tenant"
)

func env(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" && v != "unset" {
		return v
	}
	return def
}

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))
	if err := run(); err != nil {
		slog.Error("provision", "err", err)
		os.Exit(1)
	}
}

func run() error {
	slug, action := env("TENANT", ""), env("ACTION", "create")
	if tenant.ValidSlug(slug) != nil || (action != "create" && action != "sync" && action != "delete") {
		return fmt.Errorf("%w: TENANT=<slug> ACTION=create|sync|delete", errUsage)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	reg, err := tenant.Open(ctx, env("DATABASE_URL", ""))
	if err != nil {
		return err
	}
	defer reg.Close()
	t, err := reg.Tenant(ctx, slug)
	if err != nil {
		return fmt.Errorf("tenant %s: %w", slug, err)
	}
	app, err := github.NewApp(env("GH_APP_ID", ""), env("GH_APP_INSTALLATION_ID", ""), env("GH_APP_PRIVATE_KEY", ""))
	if err != nil || app == nil {
		return fmt.Errorf("GitHub App: %v", err)
	}
	org := env("TENANT_ORG", "")
	p := &Provisioner{
		Project: env("GCP_PROJECT", ""), Region: env("GCP_REGION", "us-central1"),
		Domain: env("APP_DOMAIN", "extreme.org"), Zone: env("DNS_ZONE", "extreme-org"),
		Pool: env("WIF_POOL", "extreme"), Provider: env("WIF_PROVIDER", "github"),
		Template: env("TEMPLATE_REPO", org+"/extreme-template"),
		GitHub:   github.ForApp(app),
		Token: func(ctx context.Context) (string, error) {
			tok, _, err := app.Mint(ctx)
			return tok, err
		},
		Shell: execShell{},
	}
	if action == "delete" {
		slog.Info("deleting", "tenant", slug, "repo", t.Repo)
		if err := p.Delete(ctx, t); err != nil {
			if serr := reg.SetStatus(context.WithoutCancel(ctx), slug, "deleting", err.Error()); serr != nil {
				slog.Error("record failure", "err", serr)
			}
			return err
		}
		return reg.SetStatus(ctx, slug, "deleted", "")
	}
	if err := reg.SetStatus(ctx, slug, "provisioning", ""); err != nil {
		return err
	}
	slog.Info("provisioning", "tenant", slug, "action", action, "repo", t.Repo)
	if err := p.Create(ctx, t); err != nil {
		if serr := reg.SetStatus(context.WithoutCancel(ctx), slug, "failed", err.Error()); serr != nil {
			slog.Error("record failure", "err", serr)
		}
		return err
	}
	slog.Info("provisioned", "tenant", slug)
	return reg.SetStatus(ctx, slug, "ready", "")
}

// execShell runs commands for real, logging each one.
type execShell struct{}

func (execShell) Run(ctx context.Context, env []string, args ...string) (string, error) {
	slog.Info("run", "cmd", strings.Join(args, " "))
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Env = env
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return out.String(), fmt.Errorf("%s: %w: %s", args[0], err, strings.TrimSpace(errb.String()))
	}
	return out.String(), nil
}
