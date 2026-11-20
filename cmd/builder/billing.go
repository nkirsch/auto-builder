package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/nkirsch/auto-builder/internal/billing"
	"github.com/nkirsch/auto-builder/internal/db"
	"github.com/nkirsch/auto-builder/internal/tenant"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

// billingRT is the hub's billing (TENANCY.md "Billing"), nil unless BILLING=on.
type billingRT struct {
	label   string                                                                 // BILLING_LABEL, also the Stripe idempotency prefix
	cost    func(ctx context.Context, month time.Time) (map[string]float64, error) // net cost by label value
	stripe  *billing.Stripe                                                        // nil without STRIPE_SECRET_KEY
	webhook string                                                                 // STRIPE_WEBHOOK_SECRET
}

// billingEvery spaces out recomputing statements on the tick.
const billingEvery = 24 * time.Hour

// invoiceDays is how long a payer has to pay an invoice.
const invoiceDays = 14

func newBilling(ctx context.Context, cfg config) (*billingRT, error) {
	ts, err := google.DefaultTokenSource(ctx, "https://www.googleapis.com/auth/bigquery.readonly")
	if err != nil {
		return nil, err
	}
	e := &billing.Export{Project: cfg.BillingProject, Table: cfg.BillingTable, Label: cfg.BillingLabel, Client: oauth2.NewClient(ctx, ts)}
	if err := e.Validate(); err != nil {
		return nil, err
	}
	b := &billingRT{label: cfg.BillingLabel, cost: e.Cost, webhook: cfg.StripeWebhookSecret}
	if cfg.StripeKey != "" {
		b.stripe = &billing.Stripe{Key: cfg.StripeKey}
	}
	return b, nil
}

// billingRoutes adds the platform admins' billing pages and Stripe's webhook.
func (h *hub) billingRoutes(mux *http.ServeMux) {
	if h.billing == nil {
		return
	}
	mux.HandleFunc("GET /admin/billing", h.platformAdmin(h.billingPage))
	mux.HandleFunc("POST /admin/billing/terms", h.platformAdmin(h.setBillingTerms))
	mux.HandleFunc("POST /admin/billing/recompute", h.platformAdmin(h.recomputeBilling))
	mux.HandleFunc("POST /admin/billing/accounts", h.platformAdmin(h.addAccount))
	mux.HandleFunc("POST /admin/billing/accounts/{id}", h.platformAdmin(h.editAccount))
	mux.HandleFunc("POST /admin/billing/accounts/{id}/delete", h.platformAdmin(h.deleteAccount))
	mux.HandleFunc("POST /admin/billing/accounts/{id}/send", h.platformAdmin(h.sendInvoice))
	mux.HandleFunc("POST /admin/billing/apps/{slug}", h.platformAdmin(h.setAppAccount))
	if h.billing.webhook != "" {
		mux.HandleFunc("POST /api/stripe/webhook", h.stripeWebhook)
	}
}

// syncBilling recomputes this and last month's drafts at most once per
// billingEvery, and finalizes last month from billing.FinalDay on.
func (h *hub) syncBilling(ctx context.Context, now time.Time) error {
	if h.billing == nil {
		return nil
	}
	s, err := h.reg.BillingSettings(ctx)
	if err != nil {
		return err
	}
	if s.ComputedAt != nil && now.Sub(*s.ComputedAt) < billingEvery {
		return nil
	}
	return h.computeBilling(ctx, now)
}

// computeBilling recomputes this and last month's drafts now.
func (h *hub) computeBilling(ctx context.Context, now time.Time) error {
	this := billing.Month(now)
	last := this.AddDate(0, -1, 0)
	for _, m := range []time.Time{last, this} {
		if err := h.computeMonth(ctx, m); err != nil {
			return fmt.Errorf("%s: %w", m.Format("Jan 2006"), err)
		}
	}
	if now.In(billing.Pacific).Day() >= billing.FinalDay {
		if err := h.reg.Finalize(ctx, last); err != nil {
			return err
		}
	}
	return h.reg.SetBillingComputed(ctx, now)
}

// computeMonth saves one month's draft statement for every app with cost
// or a billing account.
func (h *hub) computeMonth(ctx context.Context, month time.Time) error {
	cloud, err := h.billing.cost(ctx, month)
	if err != nil {
		return err
	}
	tenants, err := h.reg.Tenants(ctx)
	if err != nil {
		return err
	}
	accounts, err := h.reg.Accounts(ctx)
	if err != nil {
		return err
	}
	byID := map[int]tenant.Account{}
	for _, a := range accounts {
		byID[a.ID] = a
	}
	set, err := h.reg.BillingSettings(ctx)
	if err != nil {
		return err
	}
	costs := map[string]billing.Costs{}
	seconds := map[string]int64{}
	for _, t := range tenants {
		st, err := h.reg.Store(ctx, t.Slug)
		if err != nil {
			return fmt.Errorf("%s: %w", t.Slug, err)
		}
		ai, secs, err := st.RunUsage(ctx, month, month.AddDate(0, 1, 0))
		if err != nil {
			return fmt.Errorf("%s: %w", t.Slug, err)
		}
		costs[t.Slug] = billing.Costs{AI: ai, Cloud: cloud[t.Slug], RunSeconds: secs}
		seconds[t.Slug] = secs
	}
	runner := billing.SplitRunner(cloud[billing.RunnerValue], seconds)
	for _, t := range tenants {
		c := costs[t.Slug]
		c.Runner = runner[t.Slug]
		a, billed := byID[t.BillingAccount]
		if !billed && c == (billing.Costs{}) {
			continue
		}
		// Unbilled apps show their raw cost.
		var terms billing.Terms
		if billed {
			terms = billing.Terms{MarkupPct: set.MarkupPct, BaseFee: set.BaseFee}
			if a.MarkupPct != nil {
				terms.MarkupPct = *a.MarkupPct
			}
			if a.BaseFee != nil {
				terms.BaseFee = *a.BaseFee
			}
		}
		p := billing.Price(c, terms)
		if err := h.reg.SaveDraft(ctx, tenant.Statement{Tenant: t.Slug, Month: month, Account: t.BillingAccount,
			AIUSD: c.AI, RunnerUSD: c.Runner, CloudUSD: c.Cloud, RunSeconds: c.RunSeconds,
			MarkupPct: terms.MarkupPct, BaseFee: terms.BaseFee,
			AICents: p.AI, RunnerCents: p.Runner, CloudCents: p.Cloud, FeeCents: p.Fee, TotalCents: p.Total}); err != nil {
			return fmt.Errorf("%s: %w", t.Slug, err)
		}
	}
	return nil
}

// billingMonth is one month's statements on /admin/billing.
type billingMonth struct {
	Month      time.Time
	Statements []tenant.Statement
	Total      int64
}

// billingAccount is an account on /admin/billing with what Send would invoice.
type billingAccount struct {
	tenant.Account
	Unbilled int64 // cents in final statements not yet invoiced
}

func (h *hub) billingPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	set, err := h.reg.BillingSettings(ctx)
	if err != nil {
		h.fail(w, "billing settings", err)
		return
	}
	accounts, err := h.reg.Accounts(ctx)
	if err != nil {
		h.fail(w, "billing accounts", err)
		return
	}
	var rows []billingAccount
	for _, a := range accounts {
		st, err := h.reg.Unbilled(ctx, a.ID)
		if err != nil {
			h.fail(w, "unbilled", err)
			return
		}
		row := billingAccount{Account: a}
		for _, s := range st {
			row.Unbilled += s.TotalCents
		}
		rows = append(rows, row)
	}
	tenants, err := h.reg.Tenants(ctx)
	if err != nil {
		h.fail(w, "tenants", err)
		return
	}
	tenants = slices.DeleteFunc(tenants, func(t tenant.Tenant) bool { return t.Status == "deleted" })
	stmts, err := h.reg.Statements(ctx, billing.Month(time.Now()).AddDate(0, -5, 0))
	if err != nil {
		h.fail(w, "statements", err)
		return
	}
	var months []billingMonth
	for _, s := range stmts {
		if len(months) == 0 || !months[len(months)-1].Month.Equal(s.Month) {
			months = append(months, billingMonth{Month: s.Month})
		}
		m := &months[len(months)-1]
		m.Statements = append(m.Statements, s)
		m.Total += s.TotalCents
	}
	h.render(w, r, "billing", "Billing", map[string]any{"Settings": set, "Accounts": rows, "Tenants": tenants,
		"Months": months, "Stripe": h.billing.stripe != nil, "Label": h.billing.label, "Min": billing.MinInvoiceCents})
}

func (h *hub) fail(w http.ResponseWriter, what string, err error) {
	slog.Error(what, "err", err)
	http.Error(w, "internal error", http.StatusInternalServerError)
}

// billingBack returns to /admin/billing with a message.
func billingBack(w http.ResponseWriter, r *http.Request, msg string) {
	http.Redirect(w, r, "/admin/billing?flash="+url.QueryEscape(msg), http.StatusSeeOther)
}

// optAmount reads a blank-or-non-negative number: nil for blank.
func optAmount(s string) (*float64, error) {
	s = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(s), "$"), "%"))
	if s == "" {
		return nil, nil
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || !(v >= 0 && v < 1e6) {
		return nil, errors.New("bad amount")
	}
	return &v, nil
}

func (h *hub) setBillingTerms(w http.ResponseWriter, r *http.Request) {
	markup, err1 := optAmount(r.FormValue("markup"))
	fee, err2 := optAmount(r.FormValue("fee"))
	if err1 != nil || err2 != nil || markup == nil || fee == nil {
		billingBack(w, r, "Enter a markup (%) and a monthly fee ($), 0 or more")
		return
	}
	if err := h.reg.SetBillingTerms(r.Context(), *markup, *fee); err != nil {
		h.fail(w, "billing terms", err)
		return
	}
	billingBack(w, r, fmt.Sprintf("Default terms: %g%% markup, $%.2f a month per app; drafts take them at the next recompute", *markup, *fee))
}

func (h *hub) recomputeBilling(w http.ResponseWriter, r *http.Request) {
	if err := h.computeBilling(r.Context(), time.Now()); err != nil {
		slog.Error("recompute billing", "err", err)
		billingBack(w, r, "Recompute failed: "+err.Error())
		return
	}
	billingBack(w, r, "Recomputed this and last month's drafts")
}

func (h *hub) addAccount(w http.ResponseWriter, r *http.Request) {
	kind := r.FormValue("kind")
	id, err := h.reg.CreateAccount(r.Context(), tenant.Account{Kind: kind, Identity: r.FormValue("identity"),
		Name: r.FormValue("name"), Email: r.FormValue("email")})
	if err != nil {
		billingBack(w, r, "Couldn't add the account: "+err.Error())
		return
	}
	if slug := r.FormValue("app"); slug != "" {
		if err := h.reg.SetTenantAccount(r.Context(), slug, id); err != nil {
			billingBack(w, r, "Added the account, but couldn't bill "+slug+" to it: "+err.Error())
			return
		}
	}
	billingBack(w, r, "Added the account")
}

func (h *hub) accountOf(w http.ResponseWriter, r *http.Request) (tenant.Account, bool) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return tenant.Account{}, false
	}
	a, err := h.reg.BillingAccount(r.Context(), id)
	if errors.Is(err, db.ErrNotFound) {
		http.NotFound(w, r)
		return a, false
	}
	if err != nil {
		h.fail(w, "billing account", err)
		return a, false
	}
	return a, true
}

func (h *hub) editAccount(w http.ResponseWriter, r *http.Request) {
	a, ok := h.accountOf(w, r)
	if !ok {
		return
	}
	markup, err1 := optAmount(r.FormValue("markup"))
	fee, err2 := optAmount(r.FormValue("fee"))
	if err1 != nil || err2 != nil {
		billingBack(w, r, "Leave markup and fee blank for the defaults, or enter 0 or more")
		return
	}
	a.Name, a.Email, a.MarkupPct, a.BaseFee = r.FormValue("name"), r.FormValue("email"), markup, fee
	if err := h.reg.UpdateAccount(r.Context(), a); err != nil {
		h.fail(w, "update account", err)
		return
	}
	billingBack(w, r, "Saved "+a.Label())
}

func (h *hub) deleteAccount(w http.ResponseWriter, r *http.Request) {
	a, ok := h.accountOf(w, r)
	if !ok {
		return
	}
	if err := h.reg.DeleteAccount(r.Context(), a.ID); err != nil {
		h.fail(w, "delete account", err)
		return
	}
	billingBack(w, r, "Deleted "+a.Label()+"; its apps aren't billed")
}

func (h *hub) setAppAccount(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	id, err := strconv.Atoi(cmp.Or(r.FormValue("account"), "0"))
	if err != nil {
		http.Error(w, "bad account", http.StatusBadRequest)
		return
	}
	if err := h.reg.SetTenantAccount(r.Context(), slug, id); err != nil {
		billingBack(w, r, "Couldn't change "+slug+": "+err.Error())
		return
	}
	billingBack(w, r, slug+"'s billing changed; drafts follow at the next recompute")
}

func (h *hub) sendInvoice(w http.ResponseWriter, r *http.Request) {
	a, ok := h.accountOf(w, r)
	if !ok {
		return
	}
	inv, err := h.invoice(r.Context(), a)
	if err != nil {
		slog.Error("send invoice", "account", a.ID, "err", err)
		billingBack(w, r, a.Label()+": "+err.Error())
		return
	}
	billingBack(w, r, "Sent "+a.Label()+" invoice "+inv.ID+" "+inv.URL)
}

// invoice puts an account's final, uninvoiced statements on one Stripe
// invoice and emails it. Every call is idempotent, so a failed Send can be
// pressed again: the draft invoice is recorded on the statements first.
func (h *hub) invoice(ctx context.Context, a tenant.Account) (billing.Invoice, error) {
	s := h.billing.stripe
	if s == nil {
		return billing.Invoice{}, errors.New("STRIPE_SECRET_KEY isn't set")
	}
	if a.Email == "" {
		return billing.Invoice{}, errors.New("the account needs an email to invoice")
	}
	stmts, err := h.reg.Unbilled(ctx, a.ID)
	if err != nil {
		return billing.Invoice{}, err
	}
	var total int64
	draft := ""
	for _, st := range stmts {
		total += st.TotalCents
		draft = cmp.Or(draft, st.StripeInvoice)
	}
	if total < billing.MinInvoiceCents {
		return billing.Invoice{}, fmt.Errorf("$%.2f is under Stripe's minimum; it carries into the next invoice", float64(total)/100)
	}
	key := func(parts ...string) string { return h.billing.label + "-" + strings.Join(parts, "-") }
	if a.StripeCustomer == "" {
		if a.StripeCustomer, err = s.Customer(ctx, key("customer", strconv.Itoa(a.ID)), a.Label(), a.Email); err != nil {
			return billing.Invoice{}, err
		}
		if err := h.reg.SetStripeCustomer(ctx, a.ID, a.StripeCustomer); err != nil {
			return billing.Invoice{}, err
		}
	}
	if draft == "" {
		first, lastM := stmts[0].Month, stmts[len(stmts)-1].Month
		desc := h.host + " hosting, " + first.Format("Jan 2006")
		if !lastM.Equal(first) {
			desc += " – " + lastM.Format("Jan 2006")
		}
		if draft, err = s.Draft(ctx, key("invoice", strconv.Itoa(a.ID), first.Format("200601"), lastM.Format("200601")),
			a.StripeCustomer, desc, invoiceDays); err != nil {
			return billing.Invoice{}, err
		}
	}
	if err := h.reg.SetDraftInvoice(ctx, a.ID, stmts, draft); err != nil {
		return billing.Invoice{}, err
	}
	for _, st := range stmts {
		m := st.Month.Format("Jan 2006")
		for _, l := range []struct {
			part  string
			cents int64
			desc  string
		}{
			{"ai", st.AICents, "AI agent"}, {"runner", st.RunnerCents, "agent runner"},
			{"cloud", st.CloudCents, "cloud hosting"}, {"fee", st.FeeCents, "monthly fee"},
		} {
			if l.cents <= 0 {
				continue
			}
			if err := s.AddLine(ctx, key(draft, st.Tenant, st.Month.Format("200601"), l.part), a.StripeCustomer, draft,
				billing.Line{Description: st.Tenant + " · " + m + " · " + l.desc, Cents: l.cents}); err != nil {
				return billing.Invoice{}, err
			}
		}
	}
	inv, err := s.Send(ctx, draft)
	if err != nil {
		return inv, err
	}
	_, err = h.reg.SetInvoiceStatus(ctx, draft, "invoiced")
	return inv, err
}

// stripeWebhook records invoices paid, voided or marked uncollectible:
// POST /api/stripe/webhook, signed with STRIPE_WEBHOOK_SECRET.
func (h *hub) stripeWebhook(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
		return
	}
	ev, err := billing.ParseWebhook(body, r.Header.Get("Stripe-Signature"), h.billing.webhook, time.Now())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if status := billing.InvoiceStatus(ev.Type); status != "" && ev.Invoice != "" {
		n, err := h.reg.SetInvoiceStatus(r.Context(), ev.Invoice, status)
		if err != nil {
			h.fail(w, "invoice status", err)
			return
		}
		slog.Info("stripe invoice", "invoice", ev.Invoice, "status", status, "statements", n)
	}
	w.WriteHeader(http.StatusNoContent)
}

// billPersonal bills a new app to its owner's personal account, made on
// first use. It runs whether or not BILLING is on, so turning it on later
// finds each owner's app already theirs.
func (h *hub) billPersonal(ctx context.Context, slug, identity, email string) error {
	id, err := h.reg.CreateAccount(ctx, tenant.Account{Kind: "personal", Identity: identity, Email: email})
	if err != nil {
		return err
	}
	return h.reg.SetTenantAccount(ctx, slug, id)
}
