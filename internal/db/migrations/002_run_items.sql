-- items the agent acted on (reply, PR, close, file) during a run
CREATE TABLE run_items (
    run_id     INTEGER NOT NULL REFERENCES runs (id) ON DELETE CASCADE,
    item_id    INTEGER NOT NULL REFERENCES items (id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (run_id, item_id)
);
