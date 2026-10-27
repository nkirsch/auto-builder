package board

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"

	"github.com/nkirsch/auto-builder/internal/auth"
	"github.com/nkirsch/auto-builder/internal/db"
)

// Platform is who a board's admins escalate to: the hub's platform admins in
// multi-tenant mode. A single board has none.
type Platform interface {
	// Admins are the platform admins' GitHub logins.
	Admins(ctx context.Context) ([]string, error)
	// Escalated tells the platform admins an admin escalated it.
	Escalated(ctx context.Context, it db.Item, by string)
}

// assignee is how an item records a member it's assigned to: the GitHub
// login, so the item's issue can be assigned to them, or for a member who
// signs in with Google only (whose login may be made up) their address.
func assignee(m db.Member) string {
	if !m.GitHubSignIn() {
		return m.Email
	}
	return m.Login
}

// escalate raises an item a level: a member asks the board's admins, an
// admin the platform admins. An admin's clear=true drops the escalation.
func (b *Board) escalate(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	ctx := r.Context()
	me := auth.Member(ctx)
	it, err := b.Store.Item(ctx, id)
	if err != nil {
		b.fail(w, err)
		return
	}
	back := fmt.Sprintf("/items/%d", id)
	switch {
	case r.FormValue("clear") == "true":
		if !me.IsAdmin() {
			http.Error(w, "only an admin can clear an escalation", http.StatusForbidden)
			return
		}
		if err := b.setEscalation(ctx, it, "", me.Login, nil); err != nil {
			b.fail(w, err)
			return
		}
		redirect(w, r, back, "Escalation cleared")
	case me.IsAdmin():
		if b.Platform == nil {
			redirect(w, r, back, "This board has no platform admins to escalate to")
			return
		}
		if it.Escalation == "platform" {
			redirect(w, r, back, "Already with the platform admins")
			return
		}
		admins, err := b.Platform.Admins(ctx)
		if err != nil {
			b.fail(w, err)
			return
		}
		if err := b.setEscalation(ctx, it, "platform", me.Login, admins); err != nil {
			b.fail(w, err)
			return
		}
		it.Escalation, it.EscalatedBy, it.Assignees = "platform", me.Login, admins
		b.Platform.Escalated(context.WithoutCancel(ctx), it, me.Login)
		redirect(w, r, back, "Escalated to the platform admins as urgent")
	default:
		if it.Escalation != "" {
			redirect(w, r, back, "Already escalated")
			return
		}
		members, err := b.Store.Members(ctx)
		if err != nil {
			b.fail(w, err)
			return
		}
		var admins []db.Member
		for _, m := range members {
			if m.IsAdmin() {
				admins = append(admins, m)
			}
		}
		if err := b.assignAdmins(ctx, it, me.Login, admins); err != nil {
			b.fail(w, err)
			return
		}
		b.pushover(ctx, me.Login, me.Login+" escalated: "+it.Title, "Escalated to the admins.", b.itemURL(id))
		redirect(w, r, back, "Escalated to the admins")
	}
}

// ErrNotAdmin is returned by Reassign for a login that isn't one of the board's admins.
var ErrNotAdmin = errors.New("not an admin of this board")

// Reassign hands an item escalated to the platform back to one of the
// board's admins (by is the platform admin doing it).
func (b *Board) Reassign(ctx context.Context, id int, login, by string) error {
	it, err := b.Store.Item(ctx, id)
	if err != nil {
		return err
	}
	m, err := b.Store.Member(ctx, login)
	if errors.Is(err, db.ErrNotFound) || (err == nil && !m.IsAdmin()) {
		return ErrNotAdmin
	}
	if err != nil {
		return err
	}
	return b.assignAdmins(ctx, it, by, []db.Member{m})
}

// assignAdmins escalates it to admins and notifies them on the board.
func (b *Board) assignAdmins(ctx context.Context, it db.Item, by string, admins []db.Member) error {
	var assignees []string
	for _, m := range admins {
		assignees = append(assignees, assignee(m))
	}
	if err := b.setEscalation(ctx, it, "admin", by, assignees); err != nil {
		return err
	}
	for _, m := range admins {
		if !strings.EqualFold(m.Login, by) {
			b.notify(ctx, m.Login, it.ID, by, "escalation")
		}
	}
	return nil
}

// setEscalation records the escalation, moves the item's GitHub issue to
// the new assignees who have GitHub logins, and wakes the gate (human activity).
func (b *Board) setEscalation(ctx context.Context, it db.Item, level, by string, assignees []string) error {
	if err := b.Store.Escalate(ctx, it.ID, level, by, assignees); err != nil {
		return err
	}
	if it.IssueNumber != nil && b.GitHub != nil {
		gh := func(ids []string) []string {
			return slices.DeleteFunc(slices.Clone(ids), func(s string) bool { return strings.Contains(s, "@") })
		}
		remove := slices.DeleteFunc(gh(it.Assignees), func(s string) bool { return slices.Contains(assignees, s) })
		if err := b.GitHub.Reassign(ctx, b.Repo, *it.IssueNumber, remove, gh(assignees)); err != nil {
			slog.Warn("assign issue", "item", it.ID, "issue", *it.IssueNumber, "err", err)
		}
	}
	b.Gate.Activity()
	return nil
}
