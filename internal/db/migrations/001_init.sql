CREATE TABLE members (
    login      TEXT PRIMARY KEY,
    role       TEXT NOT NULL DEFAULT 'member' CHECK (role IN ('admin', 'member')),
    name       TEXT NOT NULL DEFAULT '',
    avatar_url TEXT NOT NULL DEFAULT '',
    added_by   TEXT NOT NULL DEFAULT '',
    added_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen  TIMESTAMPTZ
);

CREATE TABLE items (
    id                SERIAL PRIMARY KEY,
    title             TEXT NOT NULL,
    body              TEXT NOT NULL DEFAULT '',
    author            TEXT NOT NULL,
    -- open → building (PR open) → merged → shipped; closed = dropped by hand
    status            TEXT NOT NULL DEFAULT 'open'
                      CHECK (status IN ('open', 'building', 'merged', 'shipped', 'closed')),
    approved          BOOLEAN NOT NULL DEFAULT false,
    approved_by       TEXT NOT NULL DEFAULT '',
    pr_number         INTEGER,
    merge_sha         TEXT NOT NULL DEFAULT '',
    close_note        TEXT NOT NULL DEFAULT '',
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- last change by a human; the gate compares it to the last run
    human_activity_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    closed_at         TIMESTAMPTZ
);
CREATE INDEX items_status ON items (status);

CREATE TABLE responses (
    id         SERIAL PRIMARY KEY,
    item_id    INTEGER NOT NULL REFERENCES items (id) ON DELETE CASCADE,
    author     TEXT NOT NULL,
    body       TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX responses_item ON responses (item_id, id);

CREATE TABLE attachments (
    id           SERIAL PRIMARY KEY,
    item_id      INTEGER NOT NULL REFERENCES items (id) ON DELETE CASCADE,
    name         TEXT NOT NULL,
    content_type TEXT NOT NULL,
    size         BIGINT NOT NULL,
    object       TEXT NOT NULL,
    uploaded_by  TEXT NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE notifications (
    id         SERIAL PRIMARY KEY,
    login      TEXT NOT NULL,
    item_id    INTEGER NOT NULL REFERENCES items (id) ON DELETE CASCADE,
    actor      TEXT NOT NULL,
    kind       TEXT NOT NULL,
    read_at    TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX notifications_unread ON notifications (login) WHERE read_at IS NULL;

CREATE TABLE runs (
    id           SERIAL PRIMARY KEY,
    status       TEXT NOT NULL DEFAULT 'requested'
                 CHECK (status IN ('requested', 'running', 'done', 'failed')),
    trigger      TEXT NOT NULL,
    requested_by TEXT NOT NULL DEFAULT '',
    execution    TEXT NOT NULL DEFAULT '',
    summary      TEXT NOT NULL DEFAULT '',
    log_object   TEXT NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    started_at   TIMESTAMPTZ,
    finished_at  TIMESTAMPTZ
);
-- at most one run in flight
CREATE UNIQUE INDEX runs_one_active ON runs ((true)) WHERE status IN ('requested', 'running');
