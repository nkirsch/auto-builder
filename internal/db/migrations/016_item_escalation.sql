-- escalation: 'admin' when a member asked the board's admins (shown
-- "escalated"), 'platform' when an admin passed it on to the platform admins
-- (shown "urgent"), '' when neither; assignees are the logins (or, for
-- Google members, addresses) it's assigned to
ALTER TABLE items ADD COLUMN escalation TEXT NOT NULL DEFAULT '' CHECK (escalation IN ('', 'admin', 'platform')),
    ADD COLUMN escalated_by TEXT NOT NULL DEFAULT '',
    ADD COLUMN escalated_at TIMESTAMPTZ,
    ADD COLUMN assignees TEXT[] NOT NULL DEFAULT '{}';
