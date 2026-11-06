# auto-builder

@.claude/board.md

## This repo

Upstream for the site forks; its own site is a redirect to the board.
Board-wide docs go in `.claude/board.md`, not here, so forks pick them up
without conflicts.

GCP project `nkirsch-auto-builder`, region `us-central1`, host
`builder.extreme.org`. `BASE_URL` has no path, so the board is at `/` and the
site isn't served. `DEPLOY_ON=release`: a merge doesn't deploy; publishing a
release does.

Claude auth: `builder-runner` uses Workload Identity Federation (rule
`auto-builder-runner-rule`); its job env carries the rule's `ANTHROPIC_*` IDs,
which `infra/bootstrap.sh` reads back from the job on later runs.
