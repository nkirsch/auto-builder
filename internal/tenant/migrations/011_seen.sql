-- each IP + browser a signed-in session used, kept as long as sign-ins: a
-- session outlives the IP it signed in from (phones, Private Relay)
CREATE TABLE seen (
    identity   TEXT NOT NULL,
    ip         TEXT NOT NULL,
    user_agent TEXT NOT NULL,
    first_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (identity, ip, user_agent)
);
CREATE INDEX seen_last_at ON seen (last_at);
