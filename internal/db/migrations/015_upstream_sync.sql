-- a fork can keep up with its upstream: the board merges new upstream commits
-- through a PR it lands on green CI; upstream_pr is the one in flight
ALTER TABLE settings ADD COLUMN upstream_sync BOOLEAN NOT NULL DEFAULT false,
    ADD COLUMN upstream_pr INTEGER;
