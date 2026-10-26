-- an admin can cancel a run started by mistake; cancelled runs don't count as failures
ALTER TABLE runs DROP CONSTRAINT runs_status_check;
ALTER TABLE runs ADD CONSTRAINT runs_status_check
    CHECK (status IN ('requested', 'running', 'done', 'failed', 'cancelled'));
