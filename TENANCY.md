# Multi-tenant mode

Multi-tenant mode runs auto-builder (see `DESIGN.md`) for many apps at once.
Its example deployment is `kirsch/extreme` at extreme.org, the name used
below. Each app is its own GitHub repo with its own board, members and spend
limit. The shared agent builds the repo, and the result is deployed to
`<app>.extreme.org`. Every app shares one GCP project
(`nkirsch-auto-builder`), one Cloud SQL instance, one runner job and one
Artifact Registry. `DESIGN.md` still describes how a single board behaves.
This file covers only what changes for multiple tenants.

Status (2026-10-25): build order step 1 done (`internal/tenant`). The code lives here, off unless `TENANCY=on`,
so single-board deploys and site forks are unchanged. `kirsch/extreme` is a
fork that owns only its settings and docs and deploys this mode. Done for it:
GitHub org `extreme-dot-org` and its App, Cloud DNS zone `extreme-org`,
extreme.org verified in Search Console, secrets `extreme-<NAME>`.

## Decisions

| Topic | Decision |
|---|---|
| Tenant | One app = one repo = one board. Slug `[a-z][a-z0-9-]{1,20}`, no trailing `-` (it's a DNS label); reserved: `www api auth admin mcp static health x new`. |
| Hosts | Hub and every board on `extreme.org` (`/` hub, `/<slug>/…` board). Apps on `<slug>.extreme.org`. Agent-written apps never share a host with the boards; the session cookie is `__Host-session` (host-only, no `Domain`), so subdomains can neither read nor toss it. |
| Sign-in | GitHub (the App's OAuth callback `https://extreme.org/auth/callback`) and Google (`/auth/google/callback`, the client shared with the single boards); one session across all boards. Membership and role are per tenant; `platform admins` (control table) can create tenants. |
| Tenant data | Postgres schema per tenant (`t_<slug>`) in database `extreme` on the existing `builder` Cloud SQL instance. The existing migrations run per schema; `db.Store` is unchanged and gets a pool with `search_path=t_<slug>` (lazy, `MaxConns` 2, idle conns closed after 1 min). Item numbers stay per tenant. Control tables live in schema `control`. |
| Runs | One active run per tenant (existing `runs_one_active`, per schema); global cap `MAX_RUNS` (default 3) across tenants. Shared job `extreme-runner`. |
| Runner auth | Per-run token instead of shared `RUNNER_TOKEN`: the launcher mints 32 random bytes, stores the SHA-256 in `control.run_tokens (hash, tenant, run_id, expires_at)` and passes `RUN_TOKEN` as a job env override. Runner API and `/mcp` resolve the token to (tenant, run), so one tenant's agent can't touch another's board. Expires when the run finishes or after 70 min. |
| GitHub | Dedicated org `extreme-dot-org` holding only tenant repos; one GitHub App installed on all its repos. Installation tokens are minted with `repositories: [<repo>]` so a run's token reaches only its repo. The platform repo `kirsch/extreme` deploys the hub and isn't a tenant at first. |
| Agent command | `build-board.md` is embedded in the runner and written to `~/.claude/commands/` in the job, so tenant repos don't carry it. The repo's `CLAUDE.md` names its own checks (the template's: `hugo --minify && go vet ./... && go test ./...`). |
| Routing | Cloud Run domain mapping per app (`<slug>.extreme.org` → service `x-<slug>`) plus a Cloud DNS CNAME to `ghs.googlehosted.com`. Certs take 15–60 min after the first mapping. |
| DNS | Registration stays at Joker; nameservers move to a Cloud DNS zone `extreme-org` in `nkirsch-auto-builder`. Apex = hub (domain mapping A/AAAA); `www` keeps its current record until replaced. |
| Shipping | `merged` → `shipped` when the app's Cloud Run service's latest ready revision has a `GIT_SHA` env that contains `merge_sha` (compare API), not the server's own SHA. The server SA gets `run.viewer`. |
| Spend | Per-tenant `settings.spend_limit_usd` as today, plus `control.settings.spend_limit_usd` across all tenants (default $50/30 days). |

## Per-app resources

Created by the provisioner (below). Each step is idempotent, and every name derives from the slug:

| Resource | Name | Grants |
|---|---|---|
| Cloud Run service | `x-<slug>` (placeholder image until first deploy) | public invoker |
| Runtime SA | `x-<slug>@` | `cloudsql.client`; `secretAccessor` on its own secrets; `storage.objectUser` on the apps bucket, IAM condition `objects/<slug>/` prefix |
| Deployer SA | `x-<slug>-deploy@` | `run.developer` on `x-<slug>`, `serviceAccountUser` on `x-<slug>@`, `artifactregistry.writer` on AR repo `apps`; WIF (pool `extreme`) bound to `attribute.repository == <org>/<repo>`, provider condition requires `refs/heads/master` |
| Database | `x_<slug>`, user `x_<slug>` | secret `x-<slug>-DATABASE_URL` |
| Bucket prefix | `gs://nkirsch-auto-builder-apps/<slug>/` | env `GCS_BUCKET`, `GCS_PREFIX` |
| Secrets | `x-<slug>-<NAME>` for names listed on the tenant's admin page | created as `unset`, then filled by hand with `gcloud secrets versions add` (v1) |
| Domain | mapping `<slug>.extreme.org`, CNAME in zone `extreme-org` | — |
| Repo | `<org>/<repo>` from template `<org>/extreme-template` (Hugo site served by a small Go server, as on the family sites; CI; deploy workflow using repo variables the provisioner sets) | App installation covers it (all repos) |

## Provisioner

`cmd/provision`, run as the Cloud Run Job `extreme-provision` with SA
`extreme-provisioner`. That SA holds the broad roles: `run.admin`,
`iam.serviceAccountAdmin`, `resourcemanager.projectIamAdmin`, `cloudsql.admin`,
`dns.admin`, `secretmanager.admin`, `storage.admin`. It runs deterministic Go
with no agent, and its only inputs are `TENANT` (slug) and `ACTION`
(`create`/`sync`/`delete`). It reads the repo and secret names from the control
DB. The internet-facing server never holds these roles. It can only start this
job (`run.jobs.runWithOverrides`), and only a platform admin can trigger that
from the hub. The job writes `control.tenants.status`
(`provisioning`/`ready`/`failed`) and its log to GCS.

Domain mappings require the caller to be a verified owner of extreme.org, so
the provisioner SA is added as an owner in Search Console (manual, once).

## Hub (`extreme.org/`)

- Signed in: your tenants (role, open items, last run, app link), and Run all
  ticks for platform admins.
- `/new` (platform admin): slug, display name, repo name (default = slug),
  optional template, starts provisioning; the tenant page shows progress.
- `/<slug>/…`: the existing board, mounted with a path prefix. Templates use
  a `{{base}}` func; handlers redirect relative to the tenant base.
- `/admin` (platform admin): tenants, global spend, platform admins.

## Control schema

```
control.tenants       (slug PK, name, repo, status, created_by, created_at, provisioned_at, error)
control.platform_admins (login PK)
control.run_tokens    (hash PK, tenant, run_id, expires_at)
control.settings      (spend_limit_usd, max_runs)
control.tenant_secrets (tenant, name, PK(tenant,name))
```

## Changes to existing code

1. `internal/tenant`: registry, pool-per-schema cache, migrations per schema,
   request context → `*db.Store`.
2. `internal/board`: path prefix (`Base`), templates via `{{base}}`.
3. `internal/auth`: `__Host-` session cookie; membership looked up per tenant;
   `next` limited to same-host paths.
4. `internal/gate`: `Tick` loops over ready tenants; launcher passes
   `TENANT`, `RUN_ID`, `RUN_TOKEN`; global cap and spend.
5. `cmd/builder`: runner API + `/mcp` authenticated by run token.
6. `internal/github`: per-repo scoped installation tokens; Shipper reads the
   app's Cloud Run revision `GIT_SHA`.
7. `cmd/runner`: embedded `build-board.md`; tenant-neutral wording.
8. `cmd/provision` + `infra/bootstrap.sh` for the platform resources
   (`extreme` service, `extreme-runner` + `extreme-provision` jobs, SAs,
   database, buckets, DNS zone, WIF pool `extreme`, AR repo `apps`).

## Manual steps

1. Create GitHub org for tenant repos and the GitHub App (callback
   `https://extreme.org/auth/callback`; repo permissions contents, pull
   requests, issues, workflows, administration (write), actions, checks
   (read); install on all org repos).
2. Run `infra/bootstrap.sh` (creates Cloud DNS zone), then set Joker's
   nameservers for extreme.org to the zone's four `ns-cloud-*` servers.
3. Verify extreme.org in Search Console; add `extreme-provisioner@` as owner.
4. Fill secrets `extreme-<NAME>` (`GITHUB_CLIENT_*`, `GOOGLE_CLIENT_*`, `GH_APP_*`, `ANTHROPIC_API_KEY`);
   unprefixed names are the single board's.

## Build order

1. Control schema + tenant registry + schema-per-tenant pools; existing
   tests pass against a `t_test` schema (CI runs the suite twice). Done.
2. Path-prefixed board, hub, auth changes; run locally with two tenants.
3. Per-run tokens, gate loop, runner changes.
4. Provisioner + template repo.
5. Bootstrap, DNS cutover, first tenant.

## Open risks

- `db-f1-micro` allows ~25 connections, shared by auto-builder, every
  tenant pool and every app database. Expect to move to `db-g1-small` past a
  few apps.
- Cloud Run domain mappings are still preview; the fallback is a global
  HTTPS LB with a serverless NEG URL mask (`<service>.extreme.org`, ~$18/mo,
  no per-app DNS).
- Agents write and deploy arbitrary app code into the shared project; the
  per-app SAs above are the isolation boundary. Nothing in an app gets
  project-level roles beyond `cloudsql.client`.
