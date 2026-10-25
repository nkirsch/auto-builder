-- when the server handed the run its one GitHub token (runner setup)
ALTER TABLE runs ADD COLUMN token_at TIMESTAMPTZ;
