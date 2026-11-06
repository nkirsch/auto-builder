package main

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"strings"
	"time"
)

// maxSpendUSD bounds a report, so one bad figure can't swamp the public total.
const maxSpendUSD = 1e9

// spendReporter is a hub allowed to report to this one; the token names it.
type spendReporter struct{ Name, Token string }

// parseSpendReporters reads SPEND_REPORTERS: space-separated name=token.
func parseSpendReporters(v string) ([]spendReporter, error) {
	var out []spendReporter
	names, tokens := map[string]bool{}, map[string]bool{}
	for _, kv := range strings.Fields(v) {
		name, token, _ := strings.Cut(kv, "=")
		if name == "" || token == "" {
			return nil, errors.New("want name=token entries")
		}
		if names[name] || tokens[token] {
			return nil, fmt.Errorf("%s: name or token repeated", name)
		}
		names[name], tokens[token] = true, true
		out = append(out, spendReporter{name, token})
	}
	return out, nil
}

// reporter is the hub whose token authorized r, or "".
func (h *hub) reporter(r *http.Request) string {
	got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return ""
	}
	name := ""
	for _, s := range h.cfg.SpendReporters {
		if subtle.ConstantTimeCompare([]byte(got), []byte(s.Token)) == 1 {
			name = s.Name
		}
	}
	return name
}

// apiSpend stores a reporting hub's cumulative spend: POST /api/spend. Each
// report replaces the last, so a retry can't count twice.
func (h *hub) apiSpend(w http.ResponseWriter, r *http.Request) {
	name := h.reporter(r)
	if name == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var in struct {
		TotalUSD *float64 `json:"total_usd"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&in); err != nil || in.TotalUSD == nil ||
		*in.TotalUSD < 0 || *in.TotalUSD >= maxSpendUSD {
		http.Error(w, "want total_usd in range", http.StatusBadRequest)
		return
	}
	if err := h.reg.SetSpendReport(r.Context(), name, *in.TotalUSD); err != nil {
		slog.Error("spend report", "hub", name, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ownSpend is every app's lifetime agent cost on this hub, deleted apps
// included: their schemas stay. A failed app fails the sum, so a partial
// figure never replaces a good one.
func (h *hub) ownSpend(ctx context.Context) (float64, error) {
	tenants, err := h.reg.Tenants(ctx)
	if err != nil {
		return 0, err
	}
	total := 0.0
	for _, t := range tenants {
		st, err := h.reg.Store(ctx, t.Slug)
		if err != nil {
			return 0, fmt.Errorf("%s: %w", t.Slug, err)
		}
		c, err := st.RunCosts(ctx)
		if err != nil {
			return 0, fmt.Errorf("%s: %w", t.Slug, err)
		}
		total += c.Total
	}
	return total, nil
}

// syncSpend runs on the tick when spend is shown here or reported up: it
// records this hub's own spend, then sends on the total with what's reported
// to it. Pages read the record, so a public visit never walks the tenants.
func (h *hub) syncSpend(ctx context.Context) error {
	if !h.cfg.SpendPublic && h.cfg.SpendReportTo == "" {
		return nil
	}
	own, err := h.ownSpend(ctx)
	if err != nil {
		return err
	}
	if err := h.reg.SetSpendReport(ctx, "", own); err != nil {
		return err
	}
	if h.cfg.SpendReportTo == "" {
		return nil
	}
	total, _, err := h.reg.SpendTotal(ctx)
	if err != nil {
		return err
	}
	return pushSpend(ctx, h.cfg.SpendReportTo, h.cfg.SpendReportToken, total)
}

// publicSpend is the total for this hub's pages, "" when it isn't shown or
// nothing has been recorded yet.
func (h *hub) publicSpend(ctx context.Context) string {
	if !h.cfg.SpendPublic {
		return ""
	}
	total, ok, err := h.reg.SpendTotal(ctx)
	if err != nil {
		slog.Warn("spend total", "err", err)
		return ""
	}
	if !ok {
		return ""
	}
	return fmt.Sprintf("$%.2f", total)
}

// apiSpendTotal serves the public total to other sites' pages, such as the
// apps' home pages: GET /api/spend, 404 unless it's shown here.
func (h *hub) apiSpendTotal(w http.ResponseWriter, r *http.Request) {
	total := 0.0
	ok := false
	if h.cfg.SpendPublic {
		var err error
		if total, ok, err = h.reg.SpendTotal(r.Context()); err != nil {
			slog.Warn("spend total", "err", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
	}
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Cache-Control", "public, max-age=300")
	writeJSON(w, http.StatusOK, map[string]float64{"total_usd": math.Round(total*100) / 100})
}

// pushSpend reports a cumulative total to the hub at base.
func pushSpend(ctx context.Context, base, token string, total float64) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	body, err := json.Marshal(map[string]float64{"total_usd": total})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", base+"/api/spend", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("%s: %s", req.URL, resp.Status)
	}
	return nil
}

// syncSpend reports a standalone board's lifetime spend up, when configured.
func (s *server) syncSpend(ctx context.Context) error {
	if s.cfg.SpendReportTo == "" {
		return nil
	}
	c, err := s.store.RunCosts(ctx)
	if err != nil {
		return err
	}
	return pushSpend(ctx, s.cfg.SpendReportTo, s.cfg.SpendReportToken, c.Total)
}
