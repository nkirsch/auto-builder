-- OAuth for the hub's /mcp: clients register themselves, and only hashes of
-- codes and tokens are stored.
CREATE TABLE oauth_clients (
    id            TEXT PRIMARY KEY,
    name          TEXT NOT NULL,
    redirect_uris TEXT[] NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE oauth_codes (
    hash         BYTEA PRIMARY KEY,
    client_id    TEXT NOT NULL REFERENCES oauth_clients (id) ON DELETE CASCADE,
    identity     TEXT NOT NULL,
    redirect_uri TEXT NOT NULL,
    challenge    TEXT NOT NULL,
    expires_at   TIMESTAMPTZ NOT NULL
);

-- grant_id ties a sign-in's access and refresh tokens together, so reusing a
-- spent refresh token can end them all.
CREATE TABLE oauth_tokens (
    hash       BYTEA PRIMARY KEY,
    kind       TEXT NOT NULL CHECK (kind IN ('access', 'refresh')),
    grant_id   TEXT NOT NULL,
    client_id  TEXT NOT NULL REFERENCES oauth_clients (id) ON DELETE CASCADE,
    identity   TEXT NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    used_at    TIMESTAMPTZ
);
CREATE INDEX oauth_tokens_grant ON oauth_tokens (grant_id);
CREATE INDEX oauth_tokens_expires ON oauth_tokens (expires_at);
