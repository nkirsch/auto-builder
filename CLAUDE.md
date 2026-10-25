# auto-builder

Self-building board: members file asks, the admin approves, a Claude Code
agent (Cloud Run Job) builds approved items into PRs that auto-merge on green
CI; the server marks items shipped once its own `GIT_SHA` contains the merge.
`DESIGN.md` is the spec — read it before changing behaviour.

## Layout

- `cmd/builder` — server: board UI, `/mcp`, runner API, `/api/gate/tick`, `/health`
- `cmd/runner` — job entrypoint: claim → App token → clone → `claude -p /build-board` → finish
- `internal/db` — pgx pool, embedded migrations (`migrations/NNN_*.sql`, append-only), all queries
- `internal/auth` — GitHub or Google OAuth (Google matches `members.email`), HMAC session cookie, `Require`/`RequireAdmin`, `Bearer`
- `internal/board` — page handlers, markdown (goldmark, raw HTML escaped), mentions
- `internal/gate` — `Decide` policy, Cloud Run Job launcher, ship-checker
- `internal/mcp` — agent tools (`list_items`, `get_item`, `reply`, `set_pr`, `set_tags`, `close_item`, `file_item`, `deploy_status`)
- `internal/github` — App JWT/installation tokens, PR, compare
- `internal/store` — GCS (JSON API) or local dir
- `web/` — embedded templates + CSS (no JS build); branding only in `templates/brand.html` + `static/brand.css`
- `.claude/commands/build-board.md` — the agent's instructions; `.claude/site.md` — the per-site part
- `config.toml`, `layouts/` — Hugo site served at `/` when `BASE_URL` has a path; here a redirect to the board, forks replace it
- `app.env` — per-site settings (name, URL, Hugo version, serve site, deploy on push or release, issue sync, local Postgres port), read by `Makefile` and the workflows
- `infra/bootstrap.sh` — idempotent GCP + GitHub setup

## Develop

```bash
make db-up      # Postgres 17 in Docker (on $DOCKER_HOST's host, else localhost; port and name from app.env)
make test       # go test ./... with TEST_DATABASE_URL
make build      # hugo into public/
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

Push to `master` (or a published release, with `DEPLOY_ON=release`; never with `none`) → `deploy.yml`: CI, amd64-only images
(no arm64: Cloud Run runs amd64) to Artifact Registry, a `vYY.M.D-N` tag passed as `VERSION`,
`gcloud run deploy $SERVICE`, `gcloud run jobs update $JOB` (repo variables; WIF auth), then Pushover if
`PUSHOVER_*` secrets are set. GCP project `nkirsch-auto-builder`, region `us-central1`, host `builder.nick.org`
(`BASE_URL` has no path, so the board is at `/` and the site isn't served).

## Forks

Site forks (`kirsch/momo`, `kirsch/katrina`, `kirsch/youkoh.org`, `nkirsch/nick.org`) are git-level forks
that `git merge upstream/master`; `kirsch/extreme` (multi-tenant, `TENANCY.md`, not deployed) merges the same way. They change only `app.env`, branding, `.claude/site.md`, docs and their
Hugo site, so upstream changes merge without conflicts; keep per-site values out of every other file.
