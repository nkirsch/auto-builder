-- Set when a member's GitHub account becomes known: the next issue sync re-reads from when they joined, then clears it.
ALTER TABLE settings ADD COLUMN issues_catchup_from timestamptz;
