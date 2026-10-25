-- PRs the agent recorded with set_pr during each run
CREATE TABLE run_prs (
    run_id     INTEGER NOT NULL REFERENCES runs (id) ON DELETE CASCADE,
    item_id    INTEGER NOT NULL REFERENCES items (id) ON DELETE CASCADE,
    pr_number  INTEGER NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (run_id, pr_number)
);
-- Backfill: credit each existing PR to the first run that touched its item.
INSERT INTO run_prs (run_id, item_id, pr_number)
SELECT DISTINCT ON (i.id) ri.run_id, i.id, i.pr_number
FROM items i JOIN run_items ri ON ri.item_id = i.id
WHERE i.pr_number IS NOT NULL
ORDER BY i.id, ri.created_at, ri.run_id;
