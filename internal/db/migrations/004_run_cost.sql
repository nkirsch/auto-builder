-- API cost Claude Code reported for the run (total_cost_usd); NULL = unknown
ALTER TABLE runs ADD COLUMN cost_usd NUMERIC(10, 4);
