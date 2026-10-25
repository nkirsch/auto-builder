# The board (shared by upstream and forks)

Upstream `nkirsch/auto-builder` owns this file; forks import it from their own
`CLAUDE.md` with `@.claude/board.md` and never edit it.

Self-building board: members file asks, the admin approves, a Claude Code
agent (Cloud Run Job) builds approved items into PRs and merges them on green
CI; the server marks items shipped once its own `GIT_SHA` contains the merge.
`DESIGN.md` is the spec — read it before changing behaviour.

## Layout

- `cmd/builder` — server: board UI, `/mcp`, runner API, `/api/gate/tick`, `/health` under `BASE_URL`'s path; the static site from `SITE_DIR` at every other path (`site.go`)
- `cmd/runner` — job entrypoint: claim → App token → clone → `claude -p /build-board` → finish
- `internal/db` — pgx pool, embedded migrations (`migrations/NNN_*.sql`, append-only), all queries
- `internal/auth` — GitHub or Google OAuth (Google matches `members.email`), HMAC session cookie, `Require`/`RequireAdmin`, `Bearer`
- `internal/board` — page handlers, markdown (goldmark, raw HTML escaped), mentions; templates link via `{{base}}`
- `internal/gate` — `Decide` policy, Cloud Run Job launcher, ship-checker
- `internal/mcp` — agent tools (`list_items`, `get_item`, `reply`, `set_pr`, `set_tags`, `close_item`, `file_item`, `deploy_status`)
- `internal/github` — App JWT/installation tokens, PR, compare; `internal/store` — GCS (JSON API) or local dir; `internal/notify` — Pushover
- `web/` — embedded templates + CSS (no JS build); branding only in `templates/brand.html` + `static/brand.css`
- `.claude/commands/build-board.md` — the agent's instructions; `.claude/site.md` — the per-site part
- `config.toml`, `layouts/`, … — the Hugo site served at `/` when `BASE_URL` has a path
- `app.defaults.env` — every per-site setting with upstream's value (name, URL, Hugo version, `SERVE_SITE`, `DEPLOY_ON` push/release/none, `ISSUE_SYNC`, local Postgres); `app.env` — this site's overrides. Read by `board.mk` and the workflows
- `board.mk` — board make targets; a site's `Makefile` includes it
- `infra/bootstrap.sh` — idempotent GCP + GitHub setup

## Develop

```bash
make db-up      # Postgres 17 in Docker (on $DOCKER_HOST's host, else localhost; port and name from app*.env)
make test       # go test ./... with TEST_DATABASE_URL
make build      # build the site into public/
DEV_LOGIN=nkirsch make run   # site at localhost:8080/, board at /iterate, signed in as DEV_LOGIN, LAUNCHER=none
```

DB tests skip without `TEST_DATABASE_URL` and truncate tables — never point it at a real database. Packages share it, so run `go test -p 1 ./...`.

## Rules

- `hugo --minify && go vet ./... && go test -p 1 ./...` before every PR. New behaviour gets a test.
- New schema = new migration file; never edit an applied one.
- Human actions bump `items.human_activity_at`; bot actions must not (the gate depends on it).
- Server-rendered HTML forms with POST-redirect-GET; no client JS frameworks.
- Secrets come from env (Secret Manager on Cloud Run); the value `unset` means not configured.

## Deploy

`deploy.yml`, on push to `master` (`DEPLOY_ON=push`), a published release (`release`), or never (`none`):
CI, amd64-only images (no arm64: Cloud Run runs amd64) to Artifact Registry, a `vYY.M.D-N` tag passed
as `VERSION`, `gcloud run deploy $SERVICE`, `gcloud run jobs update $JOB` (repo variables; WIF auth),
then Pushover if `PUSHOVER_*` secrets are set.

## Forks

Site forks (`kirsch/momo`, `kirsch/katrina`, `kirsch/youkoh.org`, `nkirsch/nick.org`) and
`kirsch/extreme` (multi-tenant, `TENANCY.md`) are git-level forks that `git merge upstream/master`.
A fork owns `app.env`, `CLAUDE.md`, `.claude/site.md`, branding, its site, and appended sections of
`DESIGN.md`; everything else is upstream's, so merges don't conflict. Upstream adds new settings to
`app.defaults.env`, board docs here, and never edits `app.env`.
