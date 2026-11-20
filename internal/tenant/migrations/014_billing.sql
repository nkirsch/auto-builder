-- Billing (TENANCY.md "Billing"): who pays for each app, and its monthly statements.
CREATE TABLE billing_accounts (
    id              SERIAL PRIMARY KEY,
    kind            TEXT NOT NULL CHECK (kind IN ('personal', 'managed')),
    -- personal: the identity it belongs to (a GitHub login or an address)
    identity        TEXT UNIQUE CHECK ((kind = 'personal') = (identity IS NOT NULL)),
    name            TEXT NOT NULL DEFAULT '',
    email           TEXT NOT NULL DEFAULT '',
    stripe_customer TEXT NOT NULL DEFAULT '',
    -- NULL takes control.settings'
    markup_pct      NUMERIC(6, 2) CHECK (markup_pct >= 0),
    base_fee_usd    NUMERIC(10, 2) CHECK (base_fee_usd >= 0),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- NULL: not billed
ALTER TABLE tenants ADD COLUMN billing_account INTEGER REFERENCES billing_accounts ON DELETE SET NULL;

ALTER TABLE settings
    ADD COLUMN markup_pct NUMERIC(6, 2) NOT NULL DEFAULT 0 CHECK (markup_pct >= 0),
    ADD COLUMN base_fee_usd NUMERIC(10, 2) NOT NULL DEFAULT 0 CHECK (base_fee_usd >= 0),
    ADD COLUMN billing_computed_at TIMESTAMPTZ;

-- one per tenant per Pacific month; costs are raw, the *_cents priced
CREATE TABLE statements (
    tenant         TEXT NOT NULL REFERENCES tenants ON DELETE CASCADE,
    month          DATE NOT NULL CHECK (extract(day FROM month) = 1),
    account        INTEGER REFERENCES billing_accounts ON DELETE SET NULL,
    ai_usd         NUMERIC(12, 4) NOT NULL DEFAULT 0,
    runner_usd     NUMERIC(12, 4) NOT NULL DEFAULT 0,
    cloud_usd      NUMERIC(12, 4) NOT NULL DEFAULT 0,
    run_seconds    BIGINT NOT NULL DEFAULT 0,
    markup_pct     NUMERIC(6, 2) NOT NULL DEFAULT 0,
    base_fee_usd   NUMERIC(10, 2) NOT NULL DEFAULT 0,
    ai_cents       BIGINT NOT NULL DEFAULT 0,
    runner_cents   BIGINT NOT NULL DEFAULT 0,
    cloud_cents    BIGINT NOT NULL DEFAULT 0,
    fee_cents      BIGINT NOT NULL DEFAULT 0,
    total_cents    BIGINT NOT NULL DEFAULT 0,
    status         TEXT NOT NULL DEFAULT 'draft'
                   CHECK (status IN ('draft', 'final', 'invoiced', 'paid', 'void', 'uncollectible')),
    stripe_invoice TEXT NOT NULL DEFAULT '',
    computed_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant, month)
);
CREATE INDEX statements_invoice ON statements (stripe_invoice) WHERE stripe_invoice <> '';
