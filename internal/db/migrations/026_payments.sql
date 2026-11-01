-- payments: members buy agent credit; each adds its amount to the spend limit for 30 days
ALTER TABLE settings ADD COLUMN payments BOOLEAN NOT NULL DEFAULT false;
CREATE TABLE payments (
    id         TEXT PRIMARY KEY, -- the processor's checkout id
    login      TEXT NOT NULL,
    amount_usd NUMERIC(10, 2) NOT NULL CHECK (amount_usd > 0),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX payments_created ON payments (created_at);
