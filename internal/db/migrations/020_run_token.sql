-- the run's own runner token (SHA-256), valid while the run is requested or running
ALTER TABLE runs ADD COLUMN token_hash BYTEA;
CREATE UNIQUE INDEX runs_token_hash ON runs (token_hash) WHERE token_hash IS NOT NULL;
