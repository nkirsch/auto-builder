-- when the runner reached each stage; setup starts at started_at (claim)
ALTER TABLE runs ADD COLUMN agent_at TIMESTAMPTZ, ADD COLUMN report_at TIMESTAMPTZ;
