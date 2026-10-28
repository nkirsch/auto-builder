# Multi-tenant mode

Multi-tenant mode runs auto-builder (see `DESIGN.md`) for many apps at once.
Its example deployment is `extreme-dot-org/extreme` at extreme.org, the name used
below. Each app is its own GitHub repo with its own board, members and spend
limit. The shared agent builds the repo, and the result is deployed to
`<app>.extreme.org`. Every app shares one GCP project
(`nkirsch-auto-builder`), one Cloud SQL instance, one runner job and one
Artifact Registry. `DESIGN.md` still describes how a single board behaves.
This file covers only what changes for multiple tenants.

Status (2026-10-26): build order steps 1–4 done, plus existing repos (below) (`internal/tenant`, `cmd/builder/tenancy.go`). The code lives here, off unless `TENANCY=on`,
so single-board deploys and site forks are unchanged. `extreme-dot-org/extreme` is a
fork that owns only its settings and docs and deploys this mode. Done for it:
GitHub org `extreme-dot-org` and its App, Cloud DNS zone `extreme-org`,
extreme.org verified in Search Console, secrets `extreme-<NAME>`.

## Decisions

| Topic | Decision |
|---|---|
| Tenant | One app = one repo = one board. Slug `[a-z][a-z0-9-]{1,20}`, no trailing `-` (it's a DNS label); reserved: `api auth admin mcp static health x new hub` (`www` is allowed: the hub is the apex, so `www.extreme.org` can be an app). |
| Hosts | Hub and every board on `extreme.org` (`/` hub, `/<slug>/…` board). Apps on `<slug>.extreme.org`. Agent-written apps never share a host with the boards; the session cookie is `__Host-session` (host-only, no `Domain`), so subdomains can neither read nor toss it. Aliases (`REDIRECT_HOSTS`, `hub.extreme.org` from `infra/tenancy.sh`'s `HUB_ALIASES`) 308 to `BASE_URL`'s host, `/health` excepted, so sign-in stays on one host. `APP_DOMAIN` (default `BASE_URL`'s host) names the apps' parent domain, so the hub can move to `hub.extreme.org` (`BASE_URL`, OAuth callbacks, tick URI, runner job's `BASE_URL`; the apex then an alias or an app) while apps stay on `<slug>.extreme.org`. The hub's menu links Feedback to `FEEDBACK_URL` (default `https://builder.extreme.org/items/new`, upstream's board; `off` hides it). |
| Sign-in | GitHub (the App's OAuth callback `https://extreme.org/auth/callback`) and Google (`/auth/google/callback`, the client shared with the single boards); one session across all boards. The session holds an identity, a GitHub login or a lower-cased Google address, and its profile goes in `control.users`; each board resolves it to a member: an address by member email, a login by member login unless that member has an email and isn't marked "GitHub too" (then Google only, as on a single board). Membership and role are per tenant. An admin's connected Gmail for emailing invites (`/auth/gmail`, callback on the hub) is kept per identity in `control.users`. Platform admins (`control.platform_admins`, login plus optional email, seeded from `ADMIN_LOGIN`/`ADMIN_EMAIL`) create tenants and become the new board's admin. |
| Creators | Besides platform admins, a creator may make one app of their own from New app (the hub's index offers it): a new repo `<org>/<slug>` with the name and slug they pick, provisioned from the template, never an existing repo; they become its board's admin (a Google creator under a login made from their address). Creators are `control.creators` (a GitHub login or an address, managed on `/admin`) plus every member of a board named in `CREATORS_FROM` (names from `SITES`, asked over `GET /api/member?identity=` with `STATS_TOKEN`; `infra/tenancy.sh` sets `builder`). Someone who already has an app can't make another: one they created (`tenants.created_by`, deleted apps aside), one they admin here, or a `SITES` board outside `CREATORS_FROM` they admin (their own site is their app). A single board with `CREATE_APP_URL` (the hub's `/new`) links it as "Your app" for members. |
| Item numbers | On every board with issue sync (single boards too), an item's number is its GitHub issue number: filing on the board opens the issue first and takes its number, and an issue filed on GitHub imports with its own. PRs share GitHub's sequence, so the board has gaps. Two items filed at once can drift apart; that's accepted. A tenant's repo starts fresh, so its board matches from #1; an existing board keeps its older numbers. |
| Tenant data | Postgres schema per tenant (`t_<slug>`) in database `extreme` on the existing `builder` Cloud SQL instance. The existing migrations run per schema; `db.Store` is unchanged and gets a pool with `search_path=t_<slug>` (lazy, `MaxConns` 2, idle conns closed after 1 min). Item numbers stay per tenant. Control tables live in schema `control`. |
| Runs | One active run per tenant (existing `runs_one_active`, per schema); global cap `MAX_RUNS` (default 3) across tenants. Shared job `extreme-runner`. |
| Runner auth | Per-run token instead of shared `RUNNER_TOKEN`: the launcher mints 32 random bytes, stores the SHA-256 in `control.run_tokens (hash, tenant, run_id, expires_at)` and passes `RUN_TOKEN` as a job env override. Runner API and `/mcp` resolve the token to (tenant, run), so one tenant's agent can't touch another's board. Expires when the run finishes or after 70 min. |
| GitHub | Dedicated org `extreme-dot-org` holding only tenant repos; one GitHub App installed on all its repos. Installation tokens are minted with `repositories: [<repo>]` so a run's token reaches only its repo. The platform repo `extreme-dot-org/extreme` deploys the hub and isn't a tenant at first. |
| Agent command | `build-board.md` is embedded in the runner and written to `~/.claude/commands/` in the job, so tenant repos don't carry it. The repo's `CLAUDE.md` names its own checks (the template's: `hugo --minify && go vet ./... && go test ./...`). |
| Routing | Cloud Run domain mapping per app (`<slug>.extreme.org` → service `x-<slug>`) plus a Cloud DNS CNAME to `ghs.googlehosted.com`. Certs take 15–60 min after the first mapping. |
| DNS | Registration stays at Joker; nameservers move to a Cloud DNS zone `extreme-org` in `nkirsch-auto-builder`. Apex = hub (domain mapping A/AAAA); `www` keeps its current record until replaced. |
| Shipping | `merged` → `shipped` when the app's Cloud Run service's latest ready revision has a `GIT_SHA` env that contains `merge_sha` (compare API), not the server's own SHA. The server SA gets `run.viewer`. |
| Spend | Per-tenant `settings.spend_limit_usd` as today, plus `control.settings.spend_limit_usd` across all tenants (default $50/30 days). |

## Existing repos

A hub can also take apps that already exist and deploy themselves (nick.org's
hub: www, meet, parties, panic, fifty). `/new` with **Existing repo** creates
the tenant `ready`, with `control.tenants.existing` set, and never starts the
provisioner (which refuses one if asked). Such an app:

- is served at `<slug>.<APP_DOMAIN>` like any app, so the slug matches its host (`www` for the platform's own site);
- needs the hub's GitHub App installed on its repo, and a `CLAUDE.md` naming its checks;
- ships when its `GET /health` JSON reports a `sha` (full `GIT_SHA`) containing the merge. The platform repo's own tenant ships on the hub's `GIT_SHA` instead, since the hub deploys with it;
- gets the board's Release button when the hub sets `RELEASES=true` (items read "merged" until a release ships them);
- gets a traffic page at `/<slug>/logs` (board admins) when `LOG_PROJECT` (default the hub's `GCP_PROJECT`) has a log view `<slug>-requests` of its Cloud Run request log that the hub's server can read (`logging.viewAccessor`);
- on Delete, only leaves the hub (`deleted`); its repo, service and DNS stay.

A single board becomes the platform repo's tenant by renaming its schema in
the same database, with the old board stopped:
`ALTER SCHEMA public RENAME TO t_<slug>; CREATE SCHEMA public;`, then adding
it at `/new` as an existing repo with that slug. Items, members and runs keep
their numbers.

## Per-app resources

Created by the provisioner (below). Each step is idempotent, and every name derives from the slug:

| Resource | Name | Grants |
|---|---|---|
| Repo | `<org>/<repo>`, created empty and private, then given the template's `master` (`<org>/extreme-template`: Hugo site served by a small Go server; CI; deploy workflow) once its variables are set, so that push is the first deploy | App installation covers it (all repos) |
| Repo variables | `GCP_PROJECT`, `GCP_REGION`, `SERVICE`, `IMAGE`, `GCP_DEPLOYER`, `GCP_WIF_PROVIDER` | App permission Variables: write |
| Cloud Run service | `x-<slug>` (placeholder `cloudrun/container/hello` until first deploy) | public invoker |
| Runtime SA | `app-<slug>@` (account IDs need 6–30 characters) | none yet |
| Deployer SA | `deploy-<slug>@` | `run.developer` on `x-<slug>`, `serviceAccountUser` on `app-<slug>@`, `artifactregistry.writer` on its own AR repo; `workloadIdentityUser` for `attribute.repository/<org>/<repo>` in pool `extreme` (the provider's condition requires `refs/heads/master`) |
| Image repo | AR `x-<slug>` (one per app, so no app can overwrite another's images) | — |
| Domain | mapping `<slug>.extreme.org`, CNAME in zone `extreme-org` | — |
| Log view | `<slug>-requests` in bucket `_Default`: `x-<slug>`'s request log, for the board's `/logs` | the hub's server reads every view (`logging.viewAccessor`) |

Later, when an app needs them: database `x_<slug>` (user `x_<slug>`, secret
`x-<slug>-DATABASE_URL`, runtime SA `cloudsql.client`), bucket prefix
`gs://nkirsch-auto-builder-apps/<slug>/`, and secrets `x-<slug>-<NAME>` from
`control.tenant_secrets`. The template needs none of them.

## Provisioner

`cmd/provision`, run as the Cloud Run Job `extreme-provision` (image
`Dockerfile.provision`: the Go binary on the gcloud image) with SA
`extreme-provisioner`. That SA holds the broad roles: `run.admin`,
`iam.serviceAccountAdmin`, `resourcemanager.projectIamAdmin`,
`artifactregistry.admin`, `dns.admin`. It runs deterministic Go with no agent,
calling `gcloud` for Google Cloud (each step describes first, so `sync` only
fills gaps) and the App for GitHub. Its only inputs are `TENANT` (slug) and
`ACTION` (`create`/`sync`/`delete`). Delete removes the app's mapping,
CNAME, service, image repo and accounts, archives its repo (code and history
stay), and marks it `deleted`; its board schema stays. Boards don't run until
their app is `ready`. It reads the repo from the control
DB. The internet-facing server never holds these roles. It can only start this
job (`run.jobs.runWithOverrides`): creating an app on the hub starts it, and a
platform admin can re-run it from `/admin`. The job writes
`control.tenants.status` (`provisioning`/`ready`/`failed` with the error); its
log is in Cloud Logging.

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
control.platform_admins (login PK, email UNIQUE)
control.users         (identity PK, name, avatar_url, last_seen)
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

All done for extreme.org (2026-10-25):

1. GitHub org `extreme-dot-org`, its private App (repo permissions
   contents, pull requests, issues, workflows, administration (write),
   actions, checks (read); installed on all org repos; `GH_APP_*`) and an
   OAuth App for sign-in (callback `https://extreme.org/auth/callback`;
   `GITHUB_CLIENT_*`), since a private App's sign-in 404s for anyone
   outside the org (2026-10-27). Template repo
   `extreme-dot-org/extreme-template` (Hugo site + Go server).
2. Cloud DNS zone `extreme-org`; Joker's nameservers point at its four
   `ns-cloud-a*` servers.
3. extreme.org verified in Search Console; `extreme-provisioner@` is an owner.
4. Secrets `extreme-<NAME>` (`GITHUB_CLIENT_*`, `GOOGLE_CLIENT_*`, `GH_APP_*`);
   unprefixed names are the single board's.
5. Claude: Workload Identity Federation, no API key. Run kirsch/home
   `infrastructure/claude-wif/provision.sh gcp extreme-runner
   extreme-runner@nkirsch-auto-builder.iam.gserviceaccount.com` (`WORKSPACE=extreme.org`)
   once; `infra/tenancy.sh` defaults `CLAUDE_FEDERATION` to its output (rule
   `extreme-runner-rule`), sets the IDs on `extreme-runner` and unmounts
   `ANTHROPIC_API_KEY` (`bootstrap.sh` does the same for `builder-runner`).
   `CLAUDE_FEDERATION=` (empty) mounts `extreme-ANTHROPIC_API_KEY` instead.
6. All-sites page (`/admin/sites`, platform admins): the hub reads its apps from
   their schemas and the boards in `SITES` (space-separated `name=board URL`) over
   HTTP — public `/version` and `/health`, plus `GET /api/stats` (open items,
   last run, 30-day spend, live-since) with the shared `STATS_TOKEN`. The secret
   lives in this project; boards in another project (kirsch-org) need a copy:
   `gcloud secrets versions access latest --secret STATS_TOKEN | gcloud secrets
   create STATS_TOKEN --project kirsch-org --data-file=-`, then mount it on each
   service as `STATS_TOKEN`.

## Build order

1. Control schema + tenant registry + schema-per-tenant pools; existing
   tests pass against a `t_test` schema (CI runs the suite twice). Done.
2. Path-prefixed board, hub, auth changes; run locally with two tenants.
   Done: `TENANCY=on` (with `TENANT_ORG`) serves the hub; runs are held.
3. Per-run tokens, gate loop, runner changes. Done: `TENANT`/`RUN_TOKEN` job
   overrides, repo-scoped App tokens (`MintFor`), `/build-board` embedded at the
   module root (`commands.go`) for repos without one, global cap and spend via
   `gate.Hold`. Tenants without a Shipper until step 4.
3a. Item numbers follow issue numbers, on every board. Done (`IssueSync.File`).
4. Provisioner + template repo. Done: `cmd/provision`, the hub starts it
   (`PROVISION_JOB`), tenant shipping reads the app's serving `GIT_SHA`.
5. Bootstrap, DNS cutover, first tenant. `infra/tenancy.sh` sets up the
   platform (hub service, runner and provision jobs, accounts, WIF pool with
   `github` and `platform` providers, tick, apex domain, the platform repo's
   deploy variables); the platform repo then sets `TENANCY=on` and
   `DEPLOY_ON=push` in its `app.env`.

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
