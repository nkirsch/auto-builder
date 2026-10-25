-- GitHub issues by members become items; issue_closed once the sync closed the issue
ALTER TABLE items ADD COLUMN issue_number INTEGER UNIQUE, ADD COLUMN issue_closed BOOLEAN NOT NULL DEFAULT false;
-- member comments imported from the item's issue
ALTER TABLE responses ADD COLUMN issue_comment_id BIGINT UNIQUE;
-- issues and comments updated before this have been imported
ALTER TABLE settings ADD COLUMN issues_synced_at TIMESTAMPTZ;
