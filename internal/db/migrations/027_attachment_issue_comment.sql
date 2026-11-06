-- the GitHub issue comment an attachment was linked from (issue sync)
ALTER TABLE attachments ADD COLUMN issue_comment_id BIGINT;
