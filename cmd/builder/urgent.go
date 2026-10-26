package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"

	"github.com/nkirsch/auto-builder/internal/board"
	"github.com/nkirsch/auto-builder/internal/db"
)

// hubPlatform is a tenant board's view of the platform admins, whom its
// admins escalate items to.
type hubPlatform struct {
	h    *hub
	slug string
}

func (p hubPlatform) Admins(ctx context.Context) ([]string, error) {
	admins, err := p.h.reg.PlatformAdmins(ctx)
	if err != nil {
		return nil, err
	}
	logins := make([]string, len(admins))
	for i, a := range admins {
		logins[i] = a.Login
	}
	return logins, nil
}

func (p hubPlatform) Escalated(ctx context.Context, it db.Item, by string) {
	if p.h.notify == nil {
		return
	}
	p.h.notify.Send(ctx, fmt.Sprintf("Urgent on %s: %s", p.slug, it.Title), by+" escalated it to the platform admins.",
		fmt.Sprintf("%s/%s/items/%d", p.h.cfg.BaseURL, p.slug, it.ID))
}

// urgentApp is one app's items escalated to the platform admins, with the
// board admins they can be handed back to.
type urgentApp struct {
	Slug, Name string
	Items      []db.Item
	Admins     []db.Member
}

// urgent lists every app's items escalated to the platform admins.
func (h *hub) urgent(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	tenants, err := h.reg.Tenants(ctx)
	if err != nil {
		slog.Error("tenants", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	var apps []urgentApp
	for _, t := range tenants {
		if t.Status == "deleted" {
			continue
		}
		st, err := h.reg.Store(ctx, t.Slug)
		if err != nil {
			slog.Error("tenant store", "tenant", t.Slug, "err", err)
			continue
		}
		items, err := st.Escalated(ctx, "platform")
		if err != nil || len(items) == 0 {
			if err != nil {
				slog.Error("escalated", "tenant", t.Slug, "err", err)
			}
			continue
		}
		a := urgentApp{Slug: t.Slug, Name: t.Name, Items: items}
		members, err := st.Members(ctx)
		if err != nil {
			slog.Error("members", "tenant", t.Slug, "err", err)
		}
		for _, m := range members {
			if m.IsAdmin() {
				a.Admins = append(a.Admins, m)
			}
		}
		apps = append(apps, a)
	}
	h.render(w, r, "urgent", "Urgent", map[string]any{"Apps": apps})
}

// reassign hands an urgent item back to one of its board's admins.
func (h *hub) reassign(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	id, err := strconv.Atoi(r.PathValue("id"))
	back := func(msg string) {
		http.Redirect(w, r, "/admin/urgent?flash="+url.QueryEscape(msg), http.StatusSeeOther)
	}
	if err != nil {
		http.NotFound(w, r)
		return
	}
	rt, err := h.tenantFor(r.Context(), slug)
	if errors.Is(err, db.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		slog.Error("tenant", "tenant", slug, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	me, _ := h.reg.PlatformAdmin(r.Context(), h.auth.SignedIn(r))
	to := r.FormValue("to")
	switch err := rt.b.Reassign(r.Context(), id, to, me.Login); {
	case errors.Is(err, db.ErrNotFound):
		back(fmt.Sprintf("%s has no item #%d", slug, id))
	case errors.Is(err, board.ErrNotAdmin):
		back(fmt.Sprintf("%q isn't an admin of %s", to, slug))
	case err != nil:
		slog.Error("reassign", "tenant", slug, "item", id, "err", err)
		back(err.Error())
	default:
		back(fmt.Sprintf("%s #%d is back with %s", slug, id, to))
	}
}
