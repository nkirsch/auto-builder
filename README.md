# auto-builder

A self-building app. It's a Go server with Postgres, Cloud Storage and a
Hugo site, deployed to Cloud Run. It also has a built-in board where members
file asks. Once an admin approves an ask, an unattended Claude Code agent
changes the app's own code, opens a pull request, merges it once CI passes,
and the change deploys itself.

**Live: [builder.extreme.org](https://builder.extreme.org)**, the board this
repo is built from.

## What it does

- **App**: Go server, Postgres on Cloud SQL (schema migrations), Cloud
  Storage for files, and a Hugo site served at `/`. Secrets come from Secret
  Manager. Deploys run through GitHub Actions and Workload Identity
  Federation, with no keys stored.
- **Board**: asks, threads, attachments, @mentions, unread markers, tags and
  blocked items; GitHub or Google sign-in; member and admin roles.
- **Agent runs**: a gate starts a Cloud Run Job when there's approved work.
  The job clones the repo, runs `claude -p /build-board`, opens PRs, merges
  them on green CI, and replies on each item.
- **Shipping**: items move from building to merged to shipped once the live
  deploy contains the merge.
- **GitHub issues**: two-way sync. Item numbers follow issue numbers.
- **Guardrails**: spend limit, one run at a time, failure backoff, and an
  agent token limited to the repo.
- **Sites**: a fork adds its own Hugo site and branding and keeps merging
  upstream. momo.kirsch.org, youkoh.org and nick.org run this way.
## Multi-tenant mode

With `TENANCY=on`, one deployment builds many apps. It runs at
[extreme.org](https://extreme.org), with
[www.extreme.org](https://www.extreme.org) as its first app.

- **Hub**: one sign-in (GitHub or Google) for every board. You see the apps
  you belong to. Platform admins create, re-provision and delete apps.
- **Apps on demand**: creating an app provisions it from a template. That
  covers a private repo, a Cloud Run service, its own image repo and
  accounts, deploy access through Workload Identity Federation, and
  `<app>.extreme.org` with DNS and TLS. The first deploy happens on its own.
- **A board per app** at `extreme.org/<app>/`, with its own members, items,
  runs and spend limit. Each app's data lives in its own Postgres schema and
  storage prefix.
- **Isolation**: each run gets a run token that reaches only its own app's
  board, and a GitHub token limited to that app's repo. Deployers can touch
  only their own app.
- **Shared agent**: one runner job. A platform-wide run cap and spend limit
  apply across all apps.
- **Shipping**: an item counts as shipped once the app's own service serves
  the merge.

## Docs

- [`DESIGN.md`](DESIGN.md): how a board behaves.
- [`TENANCY.md`](TENANCY.md): multi-tenant mode.
- [`.claude/board.md`](.claude/board.md): code layout, development and deploy.
- [`infra/bootstrap.sh`](infra/bootstrap.sh) and
  [`infra/tenancy.sh`](infra/tenancy.sh): Google Cloud and GitHub setup.
