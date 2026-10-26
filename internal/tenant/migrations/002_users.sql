-- a platform admin may sign in with Google; email pairs with login
ALTER TABLE platform_admins ADD COLUMN email TEXT UNIQUE;

-- profiles from sign-in, shared by every board; identity is a GitHub login or a Google address
CREATE TABLE users (
    identity   TEXT PRIMARY KEY,
    name       TEXT NOT NULL DEFAULT '',
    avatar_url TEXT NOT NULL DEFAULT '',
    last_seen  TIMESTAMPTZ NOT NULL DEFAULT now()
);
