-- #hashtags from the ask (lower-case), or the agent's guess when none were used
ALTER TABLE items ADD COLUMN tags TEXT[] NOT NULL DEFAULT '{}';
