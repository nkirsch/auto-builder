# auto-builder

@.claude/board.md

## This repo

Upstream for the site forks; its own site is a redirect to the board.
Board-wide docs go in `.claude/board.md`, not here, so forks pick them up
without conflicts.

GCP project `nkirsch-auto-builder`, region `us-central1`, host
`builder.extreme.org`. `BASE_URL` has no path, so the board is at `/` and the
site isn't served.

Claude auth: `builder-runner` uses Workload Identity Federation (rule
`auto-builder-runner-rule`, workspace `nick.org`); its job env carries the
`ANTHROPIC_*` IDs from kirsch/home `infrastructure/claude-wif/provision.sh gcp
auto-builder-runner builder-runner@nkirsch-auto-builder.iam.gserviceaccount.com`.
