-- one-row settings; spend_limit_usd caps agent cost over the last 30 days
CREATE TABLE settings (
    id              BOOLEAN PRIMARY KEY DEFAULT true CHECK (id),
    spend_limit_usd NUMERIC(10, 2) NOT NULL DEFAULT 20 CHECK (spend_limit_usd > 0)
);
INSERT INTO settings DEFAULT VALUES;
