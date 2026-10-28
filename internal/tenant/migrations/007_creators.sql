-- people who may create one app of their own: a GitHub login or a Google address
CREATE TABLE creators (
    identity TEXT PRIMARY KEY,
    added_by TEXT NOT NULL,
    added_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
