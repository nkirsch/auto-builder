-- the server release that claimed the run (the runner job ships with it)
ALTER TABLE runs ADD COLUMN git_sha TEXT NOT NULL DEFAULT '', ADD COLUMN version TEXT NOT NULL DEFAULT '';
