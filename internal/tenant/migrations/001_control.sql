-- Multi-tenant control tables (schema control; see TENANCY.md)
CREATE TABLE tenants (
    slug           TEXT PRIMARY KEY,
    name           TEXT NOT NULL,
    repo           TEXT NOT NULL UNIQUE,
    status         TEXT NOT NULL DEFAULT 'provisioning' CHECK (status IN ('provisioning', 'ready', 'failed')),
    created_by     TEXT NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    provisioned_at TIMESTAMPTZ,
    error          TEXT NOT NULL DEFAULT ''
);

CREATE TABLE platform_admins (
    login    TEXT PRIMARY KEY,
    added_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- SHA-256 of each run's RUN_TOKEN; the token itself is never stored
CREATE TABLE run_tokens (
    hash       BYTEA PRIMARY KEY,
    tenant     TEXT NOT NULL REFERENCES tenants ON DELETE CASCADE,
    run_id     INTEGER NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL
);

-- one row; limits across all tenants
CREATE TABLE settings (
    id              BOOLEAN PRIMARY KEY DEFAULT true CHECK (id),
    spend_limit_usd NUMERIC(10, 2) NOT NULL DEFAULT 50 CHECK (spend_limit_usd > 0),
    max_runs        INTEGER NOT NULL DEFAULT 3 CHECK (max_runs > 0)
);
INSERT INTO settings DEFAULT VALUES;

-- secret names a tenant's app reads, as x-<slug>-<NAME> in Secret Manager
CREATE TABLE tenant_secrets (
    tenant TEXT NOT NULL REFERENCES tenants ON DELETE CASCADE,
    name   TEXT NOT NULL CHECK (name ~ '^[A-Z][A-Z0-9_]{0,63}$'),
    PRIMARY KEY (tenant, name)
);
