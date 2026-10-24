# Postgres runs wherever Docker does (DOCKER_HOST=host:2375 → host).
DB_HOST  ?= $(or $(shell echo "$$DOCKER_HOST" | sed -nE 's|^(tcp://)?([^:/]+).*|\2|p'),localhost)
LOCAL_DB := postgres://builder:builder@$(DB_HOST):5432/builder?sslmode=disable
TEST_DB  := postgres://builder:builder@$(DB_HOST):5432/builder_test?sslmode=disable

.PHONY: db-up db-down run test vet

# Local Postgres 17 in Docker (matches Cloud SQL).
db-up:
	@docker start auto-builder-pg 2>/dev/null || docker run -d --name auto-builder-pg \
		-e POSTGRES_USER=builder -e POSTGRES_PASSWORD=builder -e POSTGRES_DB=builder \
		-p 5432:5432 -v auto-builder-pg:/var/lib/postgresql/data postgres:17
	@for i in $$(seq 60); do pg_isready -h $(DB_HOST) -p 5432 -q && exit 0; sleep 1; done; \
		echo "postgres not ready on $(DB_HOST):5432" >&2; exit 1
	@psql "$(LOCAL_DB)" -tAc "SELECT 1 FROM pg_database WHERE datname='builder_test'" | grep -q 1 || \
		psql "$(LOCAL_DB)" -qc "CREATE DATABASE builder_test"

db-down:
	docker stop auto-builder-pg

run: db-up
	DATABASE_URL="$(LOCAL_DB)" go run ./cmd/builder

vet:
	go vet ./...

test: db-up
	TEST_DATABASE_URL="$(TEST_DB)" go test ./...
