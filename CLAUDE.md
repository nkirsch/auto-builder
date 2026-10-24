# auto-builder

Self-building board: members file asks, the admin approves, a Claude Code
agent (Cloud Run Job) builds approved items into PRs that auto-merge on green
CI; the server marks items shipped once its own `GIT_SHA` contains the merge.
`DESIGN.md` is the spec — read it before changing behaviour.

## Layout

- `cmd/builder` — server: board UI, `/mcp`, runner API, `/api/gate/tick`, `/health`
- `cmd/runner` — job entrypoint: claim → App token → clone → `claude -p /build-board` → finish
- `internal/db` — pgx pool, embedded migrations (`migrations/NNN_*.sql`, append-only), all queries
- `internal/auth` — GitHub OAuth, HMAC session cookie, `Require`/`RequireAdmin`, `Bearer`
- `internal/board` — page handlers, markdown (goldmark, raw HTML escaped), mentions
- `internal/gate` — `Decide` policy, Cloud Run Job launcher, ship-checker
- `internal/mcp` — agent tools (`list_items`, `get_item`, `reply`, `set_pr`, `close_item`, `file_item`, `deploy_status`)
- `internal/github` — App JWT/installation tokens, PR, compare
- `internal/store` — GCS (JSON API) or local dir
- `web/` — embedded templates + CSS (no JS build)
- `.claude/commands/build-board.md` — the agent's instructions
- `infra/bootstrap.sh` — idempotent GCP + GitHub setup

## Develop

```bash
make db-up      # Postgres 17 in Docker (on $DOCKER_HOST's host, else localhost)
make test       # go test ./... with TEST_DATABASE_URL
DEV_LOGIN=nkirsch make run   # http://localhost:8080, signed in as DEV_LOGIN, LAUNCHER=none
```

DB tests skip without `TEST_DATABASE_URL` and truncate tables — never point it at a real database.

## Rules

- `go vet ./... && go test ./...` before every PR. New behaviour gets a test.
- New schema = new migration file; never edit an applied one.
- Human actions bump `items.human_activity_at`; bot actions must not (the gate depends on it).
- Server-rendered HTML forms with POST-redirect-GET; no client JS frameworks.
- Secrets come from env (Secret Manager on Cloud Run); the value `unset` means not configured.

## Deploy

Push to `master` → `deploy.yml`: CI, multi-arch images to Artifact Registry,
`gcloud run deploy builder`, `gcloud run jobs update builder-runner` (WIF auth).
GCP project `nkirsch-auto-builder`, region `us-central1`, host `builder.nick.org`.
