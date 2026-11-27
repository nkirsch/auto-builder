# Multi-tenant mode

Multi-tenant mode runs auto-builder (see `DESIGN.md`) for many apps at once.
Its example deployment is `extreme-dot-org/extreme` at extreme.org, the name used
below. Each app is its own GitHub repo with its own board, members and spend
limit. The shared agent builds the repo, and the result is deployed to
`<app>.extreme.org`. Every app shares one GCP project
(`nkirsch-auto-builder`), one Cloud SQL instance, one runner job and one
Artifact Registry. `DESIGN.md` still describes how a single board behaves.
This file covers only what changes for multiple tenants.

Status (2026-11-06): live on extreme.org, iterate.nick.org and iterate.kirsch.org; build order done, plus existing repos (below) (`internal/tenant`, `cmd/builder/tenancy.go`). The code lives here, off unless `TENANCY=on`,
so single-board deploys and site forks are unchanged. `extreme-dot-org/extreme` is a
fork that owns only its settings and docs and deploys this mode. Done for it:
GitHub org `extreme-dot-org` and its App, Cloud DNS zone `extreme-org`,
extreme.org verified in Search Console, secrets `extreme-<NAME>`.

## Decisions

| Topic | Decision |
|---|---|
| Tenant | One app = one repo = one board. Slug `[a-z][a-z0-9-]{1,20}`, no trailing `-` (it's a DNS label); reserved: the hub's paths `api auth admin mcp static health x new hub`, and names an app could pass off as the platform's (`prod`, `staging`, `master`, `main`, `ns`, `mail`, `login`, `billing` …; `tenant.reserved` has the list) (`www` is allowed: the hub is the apex, so `www.extreme.org` can be an app). |
| Hosts | Hub and every board on `extreme.org` (`/` hub, `/<slug>/…` board). Apps on `<slug>.extreme.org`. Agent-written apps never share a host with the boards; the session cookie is `__Host-session` (host-only, no `Domain`), so subdomains can neither read nor toss it. Aliases (`REDIRECT_HOSTS`, `hub.extreme.org` from `infra/tenancy.sh`'s `HUB_ALIASES`) 308 to `BASE_URL`'s host, `/health` excepted, so sign-in stays on one host. `APP_DOMAIN` (default `BASE_URL`'s host) names the apps' parent domain, so the hub can move to `hub.extreme.org` (`BASE_URL`, OAuth callbacks, tick URI, runner job's `BASE_URL`; the apex then an alias or an app) while apps stay on `<slug>.extreme.org`. The hub's menu links Feedback to `FEEDBACK_URL` (default `https://builder.extreme.org/items/new`, upstream's board; `off` hides it). |
| Sign-in | GitHub (the App's OAuth callback `https://extreme.org/auth/callback`) and Google (`/auth/google/callback`, the client shared with the single boards); one session across all boards. The session holds an identity, a GitHub login or a lower-cased Google address, and its profile goes in `control.users`; each board resolves it to a member: an address by member email, a login by member login unless that member has an email and isn't marked "GitHub too" (then Google only, as on a single board). Membership and role are per tenant. An admin's connected Gmail for emailing invites (`/auth/gmail`, callback on the hub) is kept per identity in `control.users`. Sign-in attempts at every board go in `control.signins` (the board's slug, or none); platform admins see them all at `/admin/signins`, filtered by board or identity. `GET /api/signins?days=N` (bearer `STATS_TOKEN`) lists the successful ones (identity, board, time, IP, user agent) for an outside visitors report. A single board serves the same endpoint for its own `signins` and `seen` tables. `?failed=1` adds identities a board turned away (not a member), marked `refused`. Platform admins (`control.platform_admins`, login plus optional email, seeded from `ADMIN_LOGIN`/`ADMIN_EMAIL`) create tenants and become the new board's admin. |
| Creators | Besides platform admins, a creator may make one app of their own from New app (the hub's index offers it): a new repo `<org>/<slug>` with the name and slug they pick, provisioned from the template, never an existing repo; they become its board's admin (a Google creator under a login made from their address). Creators are `control.creators` (a GitHub login or an address, managed on `/admin`) plus every member of a board named in `CREATORS_FROM` (names from `SITES`, asked over `GET /api/member?identity=` with `STATS_TOKEN`; `infra/tenancy.sh` sets `builder`). Someone who already has an app can't make another: one they created (`tenants.created_by`, deleted apps aside), one they admin here, or a `SITES` board outside `CREATORS_FROM` they admin (their own site is their app). A platform admin can make someone's app for them: New app's Owner (a GitHub login or an address) becomes the board's admin alongside the platform admin and `created_by`, so it's their one app and they find it under Apps once they sign in. A single board with `CREATE_APP_URL` (the hub's `/new`) links the hub's `/mine` beside it as "Your app" for members: their own app's board (one they created, else one they admin; Apps if several), else New app if they may make one. New app sends someone who already has an app to `/mine` too. |
| Item numbers | On every board with issue sync (single boards too), an item's number is its GitHub issue number: filing on the board opens the issue first and takes its number, and an issue filed on GitHub imports with its own. PRs share GitHub's sequence, so the board has gaps. Two items filed at once can drift apart; that's accepted. A tenant's repo starts fresh, so its board matches from #1; an existing board keeps its older numbers. |
| Tenant data | Postgres schema per tenant (`t_<slug>`) in database `extreme` on the shared Cloud SQL instance `nick-org` (project `nick-org-2019`, us-west1). The existing migrations run per schema; `db.Store` queries through a `db.Schema` on one pool shared with the control schema (`MaxConns` 6, idle conns closed after 1 min); a connection runs `SET search_path` only when it last served another schema. Item numbers stay per tenant. Control tables live in schema `control`. |
| Runs | One active run per tenant (existing `runs_one_active`, per schema); global cap `MAX_RUNS` (default 3) across tenants. Shared job `extreme-runner`. |
| Runner auth | Per-run token instead of shared `RUNNER_TOKEN`: the launcher mints 32 random bytes, stores the SHA-256 in `control.run_tokens (hash, tenant, run_id, expires_at)` and passes `RUN_TOKEN` as a job env override. Runner API and `/mcp` resolve the token to (tenant, run), so one tenant's agent can't touch another's board. Expires when the run finishes or after 70 min. |
| GitHub | Dedicated org `extreme-dot-org` holding only tenant repos; one GitHub App installed on all its repos. Installation tokens are minted with `repositories: [<repo>]` so a run's token reaches only its repo. The platform repo `extreme-dot-org/extreme` deploys the hub and isn't a tenant at first. |
| Agent command | `build-board.md` is embedded in the runner and written to `~/.claude/commands/` in the job, so tenant repos don't carry it. The repo's `CLAUDE.md` names its own checks (the template's: `hugo --minify && go vet ./... && go test ./...`). |
| Routing | Cloud Run domain mapping per app (`<slug>.extreme.org` → service `x-<slug>`) plus a Cloud DNS CNAME to `ghs.googlehosted.com`. Certs take 15–60 min after the first mapping. |
| DNS | Registration stays at Joker; nameservers move to a Cloud DNS zone `extreme-org` in `nkirsch-auto-builder`. Apex = hub (domain mapping A/AAAA); `www` keeps its current record until replaced. |
| Shipping | `merged` → `shipped` when the app's Cloud Run service's latest ready revision has a `GIT_SHA` env that contains `merge_sha` (compare API), not the server's own SHA. The server SA gets `run.viewer`. The same live SHA is what the agent's `deploy_status` reports and what `/history` marks live; the nav shows no version beside an app's name (the footer's is the hub's). |
| Spend | Per-tenant `settings.spend_limit_usd` as today, plus `control.settings.spend_limit_usd` across all tenants (default $50/30 days). |

## Existing repos

A hub can also take apps that already exist and deploy themselves (a
personal hub of existing sites). `/new` with **Existing repo** creates
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
their app is `securing` or `ready`. It reads the repo from the control
DB. The internet-facing server never holds these roles. It can only start this
job (`run.jobs.runWithOverrides`): creating an app on the hub starts it, and a
platform admin can re-run it from `/admin`. The job writes
`control.tenants.status` (`provisioning`/`securing`/`failed` with the error); its
log is in Cloud Logging. `securing` means provisioned but the domain or its
certificate (15–60 min) isn't working yet: the hub's tick requests
`https://<slug>.<domain>/` and marks the app `ready` once anything answers.
Each step's state goes in `control.tenants.steps` (JSONB, `tenant.ProvisionSteps`
order; reset when the hub starts the job), and until the app is `ready` its
board's index shows them with a progress bar, reloading every 15s, plus a
Provision (retry) button for board admins when it failed. A new (not
existing) app's board starts with an approved "Build a basic site" item by its
admin, so the first run, once the app is live, turns the starter template
into a site. Until then invites
wait too: the board's `/admin` offers no Invite / Email invite (it says why),
sending one is refused, and adding a member doesn't welcome them on GitHub.

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
- Tenant API (bearer `ADMIN_TOKEN`; unrouted without it), acting as the first
  `ADMIN_LOGIN`: `POST /api/tenants` takes `/new`'s fields as JSON (`slug`,
  `name`, `repo`, `owner`, `existing`) and answers 201 with the tenant (400
  when nothing was created, 502 when a later step failed); `GET
  /api/tenants/{slug}` is its status and provisioning steps; `POST
  /api/tenants/{slug}/members` adds a member or changes their role like the
  board's `/admin` (`login` or `email`, `role` `member`/`admin`, `github`),
  without inviting or welcoming them.
  `POST /api/tenants/{slug}/items` files an ask like the board's form
  (`title`, `body`, `approve`) and answers 201 with its `id` and `url`; `POST
  /api/tenants/{slug}/run` is the board's Run now (201 with the `run`, 409
  when a run is active, held or over the spend limit); `GET /api/hubs` is
  `/admin/hubs` as JSON, where each board's `last_activity` is its newest
  login, item, reply, run, release or new user, and `last_human_activity` leaves
  out runs, releases and the bot's items and replies. `cmd/hub` is a CLI over these (`hub`, across every
  configured hub, with `-csv` and `-hub NAME`; `hub add -run BOARD TITLE`,
  `hub run BOARD`).
- `/mcp` for people, alongside the runner's: connect with `claude mcp add --transport http <hub> <BASE_URL>/mcp`, then `/mcp` in Claude Code to sign in. The hub is its own OAuth 2.1 authorization server (`cmd/builder/oauth.go`): discovery at `/.well-known/oauth-protected-resource[/mcp]` and `/.well-known/oauth-authorization-server`, open dynamic registration at `POST /oauth/register` (public clients, https or loopback redirects), PKCE S256 required, and a consent page at `/oauth/authorize` behind the normal sign-in that names the client and where it returns to. Control tables `oauth_clients`, `oauth_codes`, `oauth_tokens` hold only hashes; access tokens last an hour, refresh tokens 30 days and rotate, and reusing a spent one more than a minute later ends its grant. To revoke someone: `DELETE FROM oauth_tokens WHERE identity = '...'`. The token prefix (`hat_`) picks the tools, so a run token only gets the agent's: `hub_mcp.go`'s `list_boards`, `overview` (platform admins), `list_items`, `get_item`, `file_item`, `reply`, `close_item`, `reopen_item`, `approve_item`, `unblock_item`, `start_run`, `release` resolve the caller's role on the board at each call, by the board's own rules, and act as them. Each hub has its own login and tokens; hubs don't share them.
- New-app pushes: creating an app (from `/new` or `POST /api/tenants`) pushes "New app: <name>" through signal (who made it, for whom, its repo; links its board), and the tick that marks a securing app `ready` pushes "App live: <name>" (links the app). Stable ids, so one push each.
- 5xx alerts: each tick, once per whole 30 minutes (5 minutes late for Logging's delay), counts 5xx in every ready app's `<slug>-requests` view and the hub's own `<K_SERVICE>-requests` (`infra/tenancy.sh`), and pushes per name through signal with a stable id when a window has 3 or more, or any after a window that also had some; a lone error doesn't push. An unreadable view is only logged.
- `/admin/traffic` (platform admin): every ready app's last 30 days (visitors, views, a per-day spark, top page) from the same cached reports as each board's `/logs`.
- `/visitors` (`visitors.<APP_DOMAIN>` redirects there; `infra/tenancy.sh` maps it): each visitor and the version they were served, from the same cached log reads as `/logs`. Platform admins see every ready app (`?app=` for one); a board admin sees only their own app at `/<slug>/visitors` (`/visitors` sends them there, or lets them pick). Each page view carries its Cloud Run revision; the revision's `GIT_SHA` names the version (its `v*` tag, else the short SHA) and the PRs merged since the previous revision (the hub's `run.viewer`, the App for history). Pseudonymous: a visitor is one browser on one network (an IPv4 address or IPv6 /64, `crawlers.Network`), an HMAC (key from `SESSION_KEY`) of network + user agent, scoped to the board (`*` on the platform view) so IDs don't match across boards; the page shows a browser·OS label, paths without queries, and external referrer hosts, minus the hub's and apps' own domain. No IPs, user agents, sign-ins or member names. Left out (here and on `/logs`): crawlers by the rules of [crawlers-go](https://github.com/nkirsch/crawlers-go), judged over each visitor's hits on every app read together: a bot user agent, 3+ 404s (not icons, images, source maps, `.txt` or `/.well-known/`), Tor exits, cloud and VPS hosts' ranges, a user agent that doesn't start like a browser's, a Chrome 15+ releases old, pages on 4+ sites in 5 seconds, 10+ pages in a minute unless referred, no CSS, JS or image loaded, and one outdated user agent spread over many networks (a fleet). A browser a hub session used in the last 90 days is trusted past the network and browser checks. The rules and ranges live in that module; refresh them by bumping it (`go get github.com/nkirsch/crawlers-go@latest`). Also left out: `HOME_HOST`'s IPs (a secret; the operator's dynamic DNS name), and every IP a platform admin or that board's admin signed in from in the last 90 days, with the IPv6 /64 around each.

## Billing

Bills each app's AI and cloud cost back to a billing account, through
Stripe invoices (`internal/billing`, `cmd/builder/billing.go`). Off unless
`BILLING=on`, and platform admins only for now: no tenant, board admin or
payer sees a page, a line or a link, and nothing reaches a payer until a
platform admin presses Send on `/admin/billing`.

- Accounts (`control.billing_accounts`): `personal` belongs to one identity
  (a GitHub login or address); an app made from New app, by a creator or by
  a platform admin for an Owner, bills to its owner's, made on first use
  (whether or not `BILLING` is on). `managed` has a name and email set by a
  platform admin (a parent, a company), need not be a member of any board,
  and may cover several apps. A tenant's `billing_account` is NULL when it
  isn't billed: existing repos and a platform admin's own apps start that
  way, and `/admin/billing` assigns any app to any account.
- Cost per app per Pacific month (Google's `invoice.month`):
  - AI: `runs.cost_usd` of runs created that month.
  - Runner: the runner job's cost (export rows labelled `<BILLING_LABEL>=_runner`)
    split by run seconds (`finished_at − started_at`) across the hub's apps.
  - Cloud: export rows labelled `<BILLING_LABEL>=<slug>`, in any project on
    the billing account, net of credits. `BILLING_LABEL` defaults to the
    hub's service name (`K_SERVICE`), so hubs sharing a billing account
    don't count each other's `www`. The provisioner labels the app's service
    and image repo (`sync` labels existing apps); an existing repo's service
    is labelled by hand in its own project, e.g. `gcloud run services update
    www --project nick-org-2019 --region us-west1 --update-labels nick-org=www`.
    Cost from before a resource was labelled isn't recoverable. The hub,
    Cloud SQL, DNS, secrets and scheduler stay platform cost.
  - Export: `BILLING_TABLE` (`kirsch-org.billing_export.gcp_billing_export_v1_*`),
    queried in `BILLING_PROJECT` (default the table's project); the hub's SA
    needs `bigquery.jobUser` there and `bigquery.dataViewer` on the dataset
    (`infra/tenancy.sh` grants both). The export lags up to a day.
- Price per statement: each cost × (1 + markup), rounded to the cent, plus
  a monthly fee per billed app; `control.settings.markup_pct` and
  `base_fee_usd` (both 0 by default), overridable per account. Unbilled apps
  show raw cost, no fee.
- Statements (`control.statements`, one per tenant per month, for apps with
  cost or an account): the tick recomputes this and last month's `draft`
  rows once a day (Recompute now does it at once); from the 3rd, last
  month's become `final` and stop changing. Each keeps the raw costs and
  terms it was priced from.
- Send (per account) puts its `final` statements on one Stripe invoice: a
  line per app, month and part (AI agent, agent runner, cloud hosting,
  monthly fee), `collection_method=send_invoice`, due in 14 days, then
  `invoiced`. The customer is made on first send from the account's name
  and email. Under $0.50 (Stripe's minimum) stays `final` and carries into
  the next invoice. Every Stripe call has an idempotency key and the draft
  invoice is recorded on the statements first, so a failed Send can be
  pressed again. `POST /api/stripe/webhook` (only with
  `STRIPE_WEBHOOK_SECRET`; signature checked, 5 min tolerance) moves
  statements to `paid`, `void` or `uncollectible` on `invoice.paid`,
  `invoice.voided`, `invoice.marked_uncollectible`. Secrets
  `$NAME-STRIPE_SECRET_KEY`, `$NAME-STRIPE_WEBHOOK_SECRET`.
- `/admin/billing` (platform admins; linked from `/admin`, and from each board's admin page for platform admins along with `/admin`): default terms,
  each app's account, accounts (name, email, overrides, amount to invoice,
  Send, Delete), and six months of statements by part and status.
- Not yet: payer-facing pages, holding runs on unpaid invoices, card on file.

## App sign-in

The hub signs people into its apps, so no app needs an OAuth client of its
own and no new host needs a Google callback: the hub's own
(`<BASE_URL>/auth/google/callback`) serves them all (`cmd/builder/sso.go`).
The hub is an OpenID Connect provider with issuer `https://auth.<APP_DOMAIN>`
(auth.extreme.org, auth.nick.org, auth.kirsch.org), mapped to the hub by
`infra/tenancy.sh` (`AUTH_HOST`).

- Client ID: the app's slug. No secret: PKCE S256 is required, and a code
  goes back only to an `https` URL on the app's own host
  (`<slug>.<APP_DOMAIN>`), for a live app.
- No consent page: apps on the hub are the platform's own. Someone already
  signed in to the hub comes straight back; otherwise the hub's sign-in page
  comes first. The app decides who it lets in (an email allowlist, say);
  being signed in isn't being a member of its board.
- Endpoints on the issuer: `/.well-known/openid-configuration`, `/authorize`
  (sends the browser to `<BASE_URL>/sso/authorize`, where the session cookie
  is), `POST /token` (`authorization_code` only), `/userinfo`, `/jwks`.
  Anything else there redirects to the hub.
- ID token: EdDSA (Ed25519, key derived from `SESSION_KEY`), good for 5
  minutes, with `iss`, `aud` (slug), `sub` (the hub identity), `nonce`, and
  `email` + `email_verified` for a Google sign-in or `login` for a GitHub
  one, plus `name` and `picture` when known. It's also the access token for
  `/userinfo`. Apps keep their own sessions.
- Codes (`control.sso_codes`, hashes only) last 2 minutes and work once.
- `hubauth` (this module, standard library only) is the app side: `hubauth.New("https://www.extreme.org")`
  derives issuer `https://auth.extreme.org` and client `www`; `Login` sends
  the browser off, `Callback` at `/auth/callback` checks state and nonce,
  trades the code and verifies the token against the JWKS.

## Control schema

```
control.tenants       (slug PK, name, repo, status, created_by, created_at, provisioned_at, error, existing, steps)
control.platform_admins (login PK, email UNIQUE)
control.users         (identity PK, name, avatar_url, last_seen)
control.run_tokens    (hash PK, tenant, run_id, expires_at)
control.settings      (spend_limit_usd, max_runs)
control.tenant_secrets (tenant, name, PK(tenant,name))
control.billing_accounts (id PK, kind personal|managed, identity UNIQUE, name, email, stripe_customer, markup_pct, base_fee_usd, created_at)
control.tenants.billing_account (→ billing_accounts, NULL = not billed)
control.statements    (tenant, month, account, ai/runner/cloud_usd, run_seconds, markup_pct, base_fee_usd, ai/runner/cloud/fee/total_cents, status, stripe_invoice, computed_at, PK(tenant,month))
control.settings      (… markup_pct, base_fee_usd, billing_computed_at)
control.sso_codes     (hash PK, client → tenants, identity, redirect_uri, challenge, nonce, expires_at)
```

## Changes to existing code

1. `internal/tenant`: registry, store-per-schema cache on a shared pool, migrations per schema,
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
5. Claude: Workload Identity Federation, no API key. Create a federation
   rule for `extreme-runner@` once and pass its IDs as `CLAUDE_FEDERATION` on
   the first `infra/tenancy.sh` run; it sets them on `extreme-runner` and
   unmounts `ANTHROPIC_API_KEY`, and later runs read them back from the job
   (`bootstrap.sh` does the same for `builder-runner`). With no IDs anywhere,
   the job mounts `extreme-ANTHROPIC_API_KEY` instead.
6. Apps page (`/`): members see their apps; platform admins see every board, the hub reads its apps from
   their schemas and the boards in `SITES` (space-separated `name=board URL`) over
   HTTP — public `/version` and `/health`, plus `GET /api/stats` (open items,
   last run, 30-day spend, live-since) with the shared `STATS_TOKEN`. The secret
   lives in this project; boards in another project need a copy:
   `gcloud secrets versions access latest --secret STATS_TOKEN | gcloud secrets
   create STATS_TOKEN --project OTHER --data-file=-`, then mount it on each
   service as `STATS_TOKEN`.
7. Public spend: a hub or standalone board with `SPEND_REPORT_TO` (a hub's URL)
   and `SPEND_REPORT_TOKEN` posts its lifetime agent cost, what's reported to it
   included, to that hub's `POST /api/spend` on each tick. Unset, it reports
   nothing. The parent's `SPEND_REPORTERS` (space-separated `name=token`, in
   secret `$NAME-SPEND_REPORTERS`) says who may report; the token names the
   reporter, and each report replaces its last. With `SPEND_PUBLIC=on`, the
   hub's sign-in and Apps pages show its own total plus those reports as one
   number: reported `runs.cost_usd`, deleted apps included, no per-hub figures.
   Public `GET /api/spend` serves it as `{"total_usd": N}` with open CORS, for
   an app's own pages (404 without `SPEND_PUBLIC` or before the first report).
   `infra/tenancy.sh` and `infra/bootstrap.sh` take these settings.
8. Hubs page (`/admin/hubs`, platform admins): every hub's boards in one place.
   Each hub serves `GET /api/overview` behind `STATS_TOKEN`: its Apps rows
   (absolute board, site and `/logs` URLs; health, version, unreleased
   commits when `RELEASES=true`, last run, failed runs in the last 7 days,
   each app's 5xx responses over the last 24h from its `<slug>-requests` view
   in `LOG_PROJECT`, counted at most every 10 minutes) with each board's
   unfinished items that need someone (escalated to the platform admins:
   urgent; to a board's admins: escalated; blocked) and a count of the rest,
   plus how far the platform repo is behind upstream and how many commits on
   its `master` the hub's live `GIT_SHA` lacks (unreleased). The page shows
   this hub's and those of `HUBS` (space-separated `name=hub URL`; this hub's
   own URL is skipped): urgent, escalated and blocked items from every board
   first, then boards whose last two or more runs failed (failing), oldest
   first within each, the count of other unfinished items, then a table per
   hub linking its Admin, Urgent and Traffic. Fields a hub that hasn't merged
   them yet leaves out show as `–`. The hub calls each hub in `HUBS` with its
   own `STATS_TOKEN`, so they must share it (copied as above), and each can
   then read the other's `/api/overview` and `/api/signins`. For one-way
   access, give a hub its own secret (`STATS_SECRET` in `infra/tenancy.sh`)
   and leave the other hubs out of its `HUBS`. `infra/tenancy.sh` sets `HUBS`
   only when it's passed.

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
5. Bootstrap, DNS cutover, first tenant. Done. `infra/tenancy.sh` sets up the
   platform (hub service, runner and provision jobs, accounts, WIF pool with
   `github` and `platform` providers, tick, apex domain, the platform repo's
   deploy variables); the platform repo then sets `TENANCY=on` and
   `DEPLOY_ON` in its `app.env` (`release` on today's hubs).

## Open risks

- `nick-org` is `db-f1-micro` with `max_connections=50`, shared by every
  hub, tenant pool and app database. Expect to move to `db-g1-small` past a
  few apps.
- Cloud Run domain mappings are still preview; the fallback is a global
  HTTPS LB with a serverless NEG URL mask (`<service>.extreme.org`, ~$18/mo,
  no per-app DNS).
- Agents write and deploy arbitrary app code into the shared project; the
  per-app SAs above are the isolation boundary. Nothing in an app gets
  project-level roles beyond `cloudsql.client`.
