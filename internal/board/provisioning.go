package board

import (
	"context"
	"net/http"
)

// Provisioning is a hub app's setup progress, shown atop its board until
// the app is ready.
type Provisioning struct {
	Status string // provisioning, securing or failed
	Error  string
	Steps  []ProvisionStep
}

// ProvisionStep is one setup step: State is pending, running, done or failed.
type ProvisionStep struct {
	Name, State, Error string
}

// Percent is the share of steps done, for the progress bar.
func (p *Provisioning) Percent() int {
	if len(p.Steps) == 0 {
		return 0
	}
	done := 0
	for _, s := range p.Steps {
		if s.State == "done" {
			done++
		}
	}
	return done * 100 / len(p.Steps)
}

// provisioning is the app's setup progress, or nil once it's ready (or on a
// single board).
func (b *Board) provisioning(ctx context.Context) *Provisioning {
	if b.Provisioning == nil {
		return nil
	}
	return b.Provisioning(ctx)
}

// reprovision re-runs a failed setup for the board's admins.
func (b *Board) reprovision(w http.ResponseWriter, r *http.Request) {
	if b.Reprovision == nil {
		http.NotFound(w, r)
		return
	}
	if err := b.Reprovision(r.Context()); err != nil {
		redirect(w, r, "/", "Provisioning didn't start: "+err.Error())
		return
	}
	redirect(w, r, "/", "Provisioning again")
}
