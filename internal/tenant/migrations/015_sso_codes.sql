-- Sign-in for apps (TENANCY.md "App sign-in"): one-time codes an app trades
-- for an ID token. Only hashes are stored.
CREATE TABLE sso_codes (
    hash         BYTEA PRIMARY KEY,
    client       TEXT NOT NULL REFERENCES tenants ON DELETE CASCADE,
    identity     TEXT NOT NULL,
    redirect_uri TEXT NOT NULL,
    challenge    TEXT NOT NULL,
    nonce        TEXT NOT NULL DEFAULT '',
    expires_at   TIMESTAMPTZ NOT NULL
);
CREATE INDEX sso_codes_expires ON sso_codes (expires_at);
