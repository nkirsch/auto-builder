package tenant

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/nkirsch/auto-builder/internal/db"
)

// Account is a billing account (TENANCY.md "Billing"): personal belongs to
// one identity, managed is a name and email set by a platform admin.
type Account struct {
	ID                 int
	Kind, Identity     string // Identity only for personal
	Name, Email        string
	StripeCustomer     string
	MarkupPct, BaseFee *float64 // nil takes the hub's
	Apps               []string // slugs billed to it
}

// Label names the account for admin pages.
func (a Account) Label() string {
	switch {
	case a.Name != "":
		return a.Name
	case a.Identity != "":
		return a.Identity
	}
	return a.Email
}

const accountCols = `a.id, a.kind, coalesce(a.identity, ''), a.name, a.email, a.stripe_customer,
	a.markup_pct::float8, a.base_fee_usd::float8,
	coalesce((SELECT array_agg(slug ORDER BY slug) FROM tenants t WHERE t.billing_account = a.id AND t.status <> 'deleted'), '{}')`

func scanAccount(row pgx.Row) (Account, error) {
	var a Account
	err := row.Scan(&a.ID, &a.Kind, &a.Identity, &a.Name, &a.Email, &a.StripeCustomer, &a.MarkupPct, &a.BaseFee, &a.Apps)
	if errors.Is(err, pgx.ErrNoRows) {
		return a, db.ErrNotFound
	}
	return a, err
}

// Accounts lists every billing account, managed then personal, by label.
func (r *Registry) Accounts(ctx context.Context) ([]Account, error) {
	rows, err := r.Control.Query(ctx, `SELECT `+accountCols+` FROM billing_accounts a
		ORDER BY a.kind, lower(coalesce(nullif(a.name, ''), a.identity, a.email)), a.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Account
	for rows.Next() {
		a, err := scanAccount(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// BillingAccount returns one account, or db.ErrNotFound.
func (r *Registry) BillingAccount(ctx context.Context, id int) (Account, error) {
	return scanAccount(r.Control.QueryRow(ctx, `SELECT `+accountCols+` FROM billing_accounts a WHERE a.id = $1`, id))
}

// CreateAccount adds an account; a personal one already held by its
// identity is returned instead, with blank name and email filled in.
func (r *Registry) CreateAccount(ctx context.Context, a Account) (int, error) {
	if a.Kind != "personal" && a.Kind != "managed" {
		return 0, errors.New("kind must be personal or managed")
	}
	a.Name, a.Email = strings.TrimSpace(a.Name), strings.ToLower(strings.TrimSpace(a.Email))
	if a.Kind == "managed" {
		if a.Name == "" && a.Email == "" {
			return 0, errors.New("a managed account needs a name or an email")
		}
		var id int
		err := r.Control.QueryRow(ctx, `INSERT INTO billing_accounts (kind, name, email, markup_pct, base_fee_usd)
			VALUES ('managed', $1, $2, $3, $4) RETURNING id`, a.Name, a.Email, a.MarkupPct, a.BaseFee).Scan(&id)
		return id, err
	}
	identity := normIdentity(a.Identity)
	if identity == "" {
		return 0, errors.New("a personal account needs an identity")
	}
	if a.Email == "" && strings.Contains(identity, "@") {
		a.Email = identity
	}
	var id int
	err := r.Control.QueryRow(ctx, `INSERT INTO billing_accounts (kind, identity, name, email)
		VALUES ('personal', $1, $2, $3)
		ON CONFLICT (identity) DO UPDATE SET
			name = CASE WHEN billing_accounts.name = '' THEN EXCLUDED.name ELSE billing_accounts.name END,
			email = CASE WHEN billing_accounts.email = '' THEN EXCLUDED.email ELSE billing_accounts.email END
		RETURNING id`, identity, a.Name, a.Email).Scan(&id)
	return id, err
}

// UpdateAccount changes an account's name, email and terms.
func (r *Registry) UpdateAccount(ctx context.Context, a Account) error {
	tag, err := r.Control.Exec(ctx, `UPDATE billing_accounts SET name = $2, email = $3, markup_pct = $4, base_fee_usd = $5
		WHERE id = $1`, a.ID, strings.TrimSpace(a.Name), strings.ToLower(strings.TrimSpace(a.Email)), a.MarkupPct, a.BaseFee)
	if err == nil && tag.RowsAffected() == 0 {
		err = db.ErrNotFound
	}
	return err
}

// DeleteAccount removes an account; its apps become not billed and its
// statements keep their amounts.
func (r *Registry) DeleteAccount(ctx context.Context, id int) error {
	tag, err := r.Control.Exec(ctx, `DELETE FROM billing_accounts WHERE id = $1`, id)
	if err == nil && tag.RowsAffected() == 0 {
		err = db.ErrNotFound
	}
	return err
}

// SetStripeCustomer records an account's Stripe customer.
func (r *Registry) SetStripeCustomer(ctx context.Context, id int, customer string) error {
	_, err := r.Control.Exec(ctx, `UPDATE billing_accounts SET stripe_customer = $2 WHERE id = $1`, id, customer)
	return err
}

// SetTenantAccount bills slug to account; 0 stops billing it.
func (r *Registry) SetTenantAccount(ctx context.Context, slug string, account int) error {
	tag, err := r.Control.Exec(ctx, `UPDATE tenants SET billing_account = nullif($2, 0) WHERE slug = $1`, slug, account)
	if err == nil && tag.RowsAffected() == 0 {
		err = db.ErrNotFound
	}
	return err
}

// BillingSettings are the hub's default terms and when statements were last computed.
type BillingSettings struct {
	MarkupPct, BaseFee float64
	ComputedAt         *time.Time
}

func (r *Registry) BillingSettings(ctx context.Context) (BillingSettings, error) {
	var s BillingSettings
	err := r.Control.QueryRow(ctx, `SELECT markup_pct::float8, base_fee_usd::float8, billing_computed_at FROM settings`).
		Scan(&s.MarkupPct, &s.BaseFee, &s.ComputedAt)
	return s, err
}

// SetBillingTerms changes the hub's default markup and fee.
func (r *Registry) SetBillingTerms(ctx context.Context, markupPct, baseFee float64) error {
	_, err := r.Control.Exec(ctx, `UPDATE settings SET markup_pct = $1, base_fee_usd = $2`, markupPct, baseFee)
	return err
}

// SetBillingComputed records when statements were last computed.
func (r *Registry) SetBillingComputed(ctx context.Context, at time.Time) error {
	_, err := r.Control.Exec(ctx, `UPDATE settings SET billing_computed_at = $1`, at)
	return err
}

// Statement is one app's month: raw costs, the terms they were priced on,
// and the priced amounts in cents.
type Statement struct {
	Tenant                           string
	Month                            time.Time
	Account                          int // 0: not billed
	AIUSD, RunnerUSD, CloudUSD       float64
	RunSeconds                       int64
	MarkupPct, BaseFee               float64
	AICents, RunnerCents, CloudCents int64
	FeeCents, TotalCents             int64
	Status, StripeInvoice            string
	ComputedAt                       time.Time
}

const statementCols = `tenant, month, coalesce(account, 0), ai_usd::float8, runner_usd::float8, cloud_usd::float8, run_seconds,
	markup_pct::float8, base_fee_usd::float8, ai_cents, runner_cents, cloud_cents, fee_cents, total_cents,
	status, stripe_invoice, computed_at`

func scanStatements(rows pgx.Rows) ([]Statement, error) {
	defer rows.Close()
	var out []Statement
	for rows.Next() {
		var s Statement
		if err := rows.Scan(&s.Tenant, &s.Month, &s.Account, &s.AIUSD, &s.RunnerUSD, &s.CloudUSD, &s.RunSeconds,
			&s.MarkupPct, &s.BaseFee, &s.AICents, &s.RunnerCents, &s.CloudCents, &s.FeeCents, &s.TotalCents,
			&s.Status, &s.StripeInvoice, &s.ComputedAt); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// monthDate is the DATE a month is stored as.
func monthDate(m time.Time) string { return m.Format("2006-01-02") }

// SaveDraft writes a month's statement unless it's no longer a draft.
func (r *Registry) SaveDraft(ctx context.Context, s Statement) error {
	_, err := r.Control.Exec(ctx, `INSERT INTO statements (tenant, month, account, ai_usd, runner_usd, cloud_usd, run_seconds,
			markup_pct, base_fee_usd, ai_cents, runner_cents, cloud_cents, fee_cents, total_cents, computed_at)
		VALUES ($1, $2::date, nullif($3, 0), $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, now())
		ON CONFLICT (tenant, month) DO UPDATE SET account = EXCLUDED.account, ai_usd = EXCLUDED.ai_usd,
			runner_usd = EXCLUDED.runner_usd, cloud_usd = EXCLUDED.cloud_usd, run_seconds = EXCLUDED.run_seconds,
			markup_pct = EXCLUDED.markup_pct, base_fee_usd = EXCLUDED.base_fee_usd, ai_cents = EXCLUDED.ai_cents,
			runner_cents = EXCLUDED.runner_cents, cloud_cents = EXCLUDED.cloud_cents, fee_cents = EXCLUDED.fee_cents,
			total_cents = EXCLUDED.total_cents, computed_at = now()
		WHERE statements.status = 'draft'`,
		s.Tenant, monthDate(s.Month), s.Account, s.AIUSD, s.RunnerUSD, s.CloudUSD, s.RunSeconds,
		s.MarkupPct, s.BaseFee, s.AICents, s.RunnerCents, s.CloudCents, s.FeeCents, s.TotalCents)
	return err
}

// Finalize turns drafts for months up to month into final statements.
func (r *Registry) Finalize(ctx context.Context, month time.Time) error {
	_, err := r.Control.Exec(ctx, `UPDATE statements SET status = 'final' WHERE status = 'draft' AND month <= $1::date`, monthDate(month))
	return err
}

// Statements lists statements from month on, newest month first.
func (r *Registry) Statements(ctx context.Context, since time.Time) ([]Statement, error) {
	rows, err := r.Control.Query(ctx, `SELECT `+statementCols+` FROM statements WHERE month >= $1::date
		ORDER BY month DESC, tenant`, monthDate(since))
	if err != nil {
		return nil, err
	}
	return scanStatements(rows)
}

// Unbilled lists an account's final statements not yet invoiced, oldest first.
func (r *Registry) Unbilled(ctx context.Context, account int) ([]Statement, error) {
	rows, err := r.Control.Query(ctx, `SELECT `+statementCols+` FROM statements
		WHERE account = $1 AND status = 'final' ORDER BY month, tenant`, account)
	if err != nil {
		return nil, err
	}
	return scanStatements(rows)
}

// SetDraftInvoice records the draft invoice an account's statements go on,
// so a retried Send reuses it.
func (r *Registry) SetDraftInvoice(ctx context.Context, account int, keys []Statement, invoice string) error {
	for _, s := range keys {
		if _, err := r.Control.Exec(ctx, `UPDATE statements SET stripe_invoice = $3
			WHERE tenant = $1 AND month = $2::date AND account = $4 AND status = 'final'`,
			s.Tenant, monthDate(s.Month), invoice, account); err != nil {
			return err
		}
	}
	return nil
}

// SetInvoiceStatus moves every statement on invoice to status: invoiced once
// sent, then what Stripe reports. A paid, void or uncollectible statement
// only moves between those.
func (r *Registry) SetInvoiceStatus(ctx context.Context, invoice, status string) (int64, error) {
	tag, err := r.Control.Exec(ctx, `UPDATE statements SET status = $2
		WHERE stripe_invoice = $1 AND (status IN ('final', 'invoiced') OR $2 IN ('paid', 'void', 'uncollectible'))`,
		invoice, status)
	return tag.RowsAffected(), err
}
