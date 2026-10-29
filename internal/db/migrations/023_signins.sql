-- sign-in attempts, kept 90 days: provider github/google, identity a login or lower-cased address, result ok or why it failed
CREATE TABLE signins (
    id         BIGSERIAL PRIMARY KEY,
    at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    provider   TEXT NOT NULL,
    identity   TEXT NOT NULL DEFAULT '',
    result     TEXT NOT NULL,
    board      TEXT NOT NULL DEFAULT '',
    ip         TEXT NOT NULL DEFAULT '',
    user_agent TEXT NOT NULL DEFAULT ''
);
CREATE INDEX signins_at ON signins (at);
CREATE INDEX signins_identity ON signins (lower(identity), at);
