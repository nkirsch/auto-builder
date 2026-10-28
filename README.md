# auto-builder

A self-building app. It's a Go server with Postgres, Cloud Storage and a
Hugo site, deployed to Cloud Run. It also has a built-in board where members
file asks. Once an admin approves an ask, an unattended Claude Code agent
changes the app's own code, opens a pull request, merges it once CI passes,
and the change deploys itself.

**Live: [builder.extreme.org](https://builder.extreme.org)**, the board this
repo is built from.

## Get started

- **Ask for a change to auto-builder.** Members sign in at
  [builder.extreme.org](https://builder.extreme.org) and file an ask; an
  admin approves it and the agent builds it. Not a member? Open an issue
  here asking to join, or ask an admin for an invite.
- **Get your own app built this way.** Members of builder.extreme.org
  follow "Your app" in its menu: pick a name and you get a repo, a site at
  `<app>.extreme.org` and its own board where you're the admin, with
  nothing to set up. One app each; others ask an admin of
  [extreme.org](https://extreme.org) to add them as a creator.
- **Run your own board.** Fork this repo, set your overrides in `app.env`,
  run [`infra/bootstrap.sh`](infra/bootstrap.sh) against your Google Cloud
  project, create the GitHub App (DESIGN.md, Manual steps), and push to
  `master`. `make merge-upstream` keeps the fork current.

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
- **Guardrails**: spend limit, one run at a time, failure backoff, a runner
  token that dies with its run, and an agent GitHub token limited to the
  repo (workflow changes only while an admin allows them).
- **Sites**: a fork can add its own Hugo site and branding and keep merging
  upstream. Most sites now sit on a hub instead (below): iterate.nick.org
  and iterate.kirsch.org hold their apps' boards.

## Multi-tenant mode

With `TENANCY=on`, one deployment builds many apps. It runs at
[extreme.org](https://extreme.org), with
[www.extreme.org](https://www.extreme.org) as its first app.

- **Hub**: one sign-in (GitHub or Google) for every board. You see the apps
  you belong to. Platform admins create, re-provision and delete apps;
  creators make one app of their own.
- **Apps on demand**: creating an app provisions it from a template. That
  covers a private repo, a Cloud Run service, its own image repo and
  accounts, deploy access through Workload Identity Federation, and
  `<app>.extreme.org` with DNS and TLS. The first deploy happens on its own.
- **A board per app** at `extreme.org/<app>/`, with its own members, items,
  runs and spend limit. Each board's data lives in its own Postgres schema
  and storage prefix.
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
