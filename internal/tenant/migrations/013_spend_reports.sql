-- Lifetime agent spend behind a hub's public total: its own (hub = '') and
-- the latest cumulative figure from each hub that reports to it.
CREATE TABLE spend_reports (
    hub         TEXT PRIMARY KEY,
    total_usd   NUMERIC(14, 4) NOT NULL CHECK (total_usd >= 0),
    reported_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
