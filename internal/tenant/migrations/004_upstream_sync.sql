-- the platform repo can keep up with its upstream, as a fork's board can
ALTER TABLE settings ADD COLUMN upstream_sync BOOLEAN NOT NULL DEFAULT false,
    ADD COLUMN upstream_pr INTEGER;
