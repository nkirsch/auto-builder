# Board targets for any site's Makefile: include board.mk.
include app.defaults.env app.env
# Postgres runs wherever Docker does (DOCKER_HOST=host:2375 → host).
DB_HOST  ?= $(or $(shell echo "$$DOCKER_HOST" | sed -nE 's|^(tcp://)?([^:/]+).*|\2|p'),localhost)
LOCAL_DB := postgres://builder:builder@$(DB_HOST):$(PG_PORT)/builder?sslmode=disable
TEST_DB  := postgres://builder:builder@$(DB_HOST):$(PG_PORT)/builder_test?sslmode=disable

.PHONY: db-up db-down run test vet merge-upstream

# Local Postgres 17 in Docker (matches Cloud SQL).
db-up:
	@docker start $(PG_NAME) 2>/dev/null || docker run -d --name $(PG_NAME) \
		-e POSTGRES_USER=builder -e POSTGRES_PASSWORD=builder -e POSTGRES_DB=builder \
		-p $(PG_PORT):5432 -v $(PG_NAME):/var/lib/postgresql/data postgres:17
	@for i in $$(seq 60); do pg_isready -h $(DB_HOST) -p $(PG_PORT) -q && exit 0; sleep 1; done; \
		echo "postgres not ready on $(DB_HOST):$(PG_PORT)" >&2; exit 1
	@psql "$(LOCAL_DB)" -tAc "SELECT 1 FROM pg_database WHERE datname='builder_test'" | grep -q 1 || \
		psql "$(LOCAL_DB)" -qc "CREATE DATABASE builder_test"

db-down:
	docker stop $(PG_NAME)

# Site at /, board at /iterate, signed in as DEV_LOGIN.
run: db-up build
	BASE_URL=http://localhost:8080/iterate SITE_DIR=public REPO=$(REPO) DATABASE_URL="$(LOCAL_DB)" go run ./cmd/builder

vet:
	go vet ./...

test: db-up
	TEST_DATABASE_URL="$(TEST_DB)" go test -p 1 ./...
	@psql "$(TEST_DB)" -qc 'CREATE SCHEMA IF NOT EXISTS t_test'
	TEST_DATABASE_URL="$(TEST_DB)&search_path=t_test" go test -p 1 ./...

# Forks: merge upstream, keeping fork-owned files (.gitattributes) on conflict.
merge-upstream:
	git config merge.ours.driver true
	git fetch upstream
	git merge -X no-renames --no-edit upstream/master
