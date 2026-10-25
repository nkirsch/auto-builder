-- why an item needs action outside auto-builder (or hit an error); '' = not blocked
ALTER TABLE items ADD COLUMN blocked TEXT NOT NULL DEFAULT '', ADD COLUMN blocked_at TIMESTAMPTZ;
