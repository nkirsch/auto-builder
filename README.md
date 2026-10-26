# auto-builder

A self-building board. Members file asks, an admin approves them, and an
unattended Claude Code agent builds each approved ask into a pull request,
merges it once CI passes, and reports back when it ships.

**Live board: [builder.extreme.org](https://builder.extreme.org)**. This
repo is built from it.

## What it does

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
- **Multi-tenant mode** (`TENANCY=on`): one hub serves a board per app. Each
  app gets its own repo, Cloud Run service and `<app>.extreme.org`,
  provisioned on demand. It runs at [extreme.org](https://extreme.org).

## Docs

- [`DESIGN.md`](DESIGN.md): how a board behaves.
- [`TENANCY.md`](TENANCY.md): multi-tenant mode.
- [`.claude/board.md`](.claude/board.md): code layout, development and deploy.
- [`infra/bootstrap.sh`](infra/bootstrap.sh) and
  [`infra/tenancy.sh`](infra/tenancy.sh): Google Cloud and GitHub setup.
