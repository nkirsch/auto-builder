// Package gate decides when a builder run is warranted and launches the
// runner job; it also moves items through merged → shipped.
package gate

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/nkirsch/auto-builder/internal/db"
)

const (
	failBackoff  = 30 * time.Minute
	staleRunning = 70 * time.Minute
	staleRequest = 15 * time.Minute
)

// Launcher starts the runner for a run and returns its execution name.
type Launcher interface {
	Launch(ctx context.Context, runID int) (string, error)
}

// Gate launches runs when the board warrants one.
type Gate struct {
	Store    *db.Store
	Launcher Launcher
	Idle     time.Duration
	Ship     *Shipper // nil disables ship checks

	mu      sync.Mutex // serialises Check so concurrent activity doesn't double-launch
	pending chan struct{}
}

// Decide is the gate's policy; trigger is "now", "activity" or "tick".
func Decide(s db.GateState, now time.Time, trigger string, idle time.Duration) (bool, string) {
	if s.Active {
		return false, "a run is active"
	}
	if trigger == "now" {
		return true, "now"
	}
	if s.LastFailure != nil && now.Sub(*s.LastFailure) < failBackoff {
		return false, "last run failed recently"
	}
	if s.NewActivity {
		return true, "activity"
	}
	if s.ApprovedOpen && (s.LastSuccess == nil || now.Sub(*s.LastSuccess) > idle) {
		return true, "idle"
	}
	return false, "nothing to do"
}

// Check evaluates the gate and launches if warranted.
func (g *Gate) Check(ctx context.Context, trigger string) (int, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	s, err := g.Store.GateState(ctx)
	if err != nil {
		return 0, err
	}
	ok, reason := Decide(s, time.Now(), trigger, g.Idle)
	if !ok {
		if trigger == "now" {
			return 0, db.ErrRunActive
		}
		slog.Debug("gate hold", "trigger", trigger, "reason", reason)
		return 0, nil
	}
	return g.launch(ctx, reason, "")
}

// RunNow launches unless a run is active (admin button).
func (g *Gate) RunNow(ctx context.Context, by string) (int, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	s, err := g.Store.GateState(ctx)
	if err != nil {
		return 0, err
	}
	if ok, _ := Decide(s, time.Now(), "now", g.Idle); !ok {
		return 0, db.ErrRunActive
	}
	return g.launch(ctx, "now", by)
}

// Activity schedules a background check; bursts of activity coalesce into one.
func (g *Gate) Activity() {
	g.mu.Lock()
	if g.pending == nil {
		g.pending = make(chan struct{}, 1)
	}
	ch := g.pending
	g.mu.Unlock()
	select {
	case ch <- struct{}{}:
	default:
		return
	}
	go func() {
		// Let a burst (item + attachments + approve) settle first.
		time.Sleep(5 * time.Second)
		<-ch
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if _, err := g.Check(ctx, "activity"); err != nil {
			slog.Error("gate activity", "err", err)
		}
	}()
}

// Tick runs the periodic duties: fail stale runs, check shipping, evaluate.
func (g *Gate) Tick(ctx context.Context) error {
	var errs []error
	if n, err := g.Store.FailStale(ctx, staleRunning, staleRequest); err != nil {
		errs = append(errs, fmt.Errorf("fail stale: %w", err))
	} else if n > 0 {
		slog.Warn("failed stale runs", "count", n)
	}
	if g.Ship != nil {
		if err := g.Ship.Check(ctx); err != nil {
			errs = append(errs, fmt.Errorf("ship: %w", err))
		}
	}
	if _, err := g.Check(ctx, "tick"); err != nil {
		errs = append(errs, fmt.Errorf("check: %w", err))
	}
	return errors.Join(errs...)
}

func (g *Gate) launch(ctx context.Context, trigger, by string) (int, error) {
	id, err := g.Store.CreateRun(ctx, trigger, by)
	if err != nil {
		return 0, err
	}
	exec, err := g.Launcher.Launch(ctx, id)
	if err != nil {
		if ferr := g.Store.FinishRun(ctx, id, "failed", "launch: "+err.Error(), ""); ferr != nil {
			slog.Error("finish run after launch failure", "run", id, "err", ferr)
		}
		return id, fmt.Errorf("launch run %d: %w", id, err)
	}
	if err := g.Store.SetExecution(ctx, id, exec); err != nil {
		slog.Warn("set execution", "run", id, "err", err)
	}
	slog.Info("launched run", "run", id, "trigger", trigger, "by", by, "execution", exec)
	return id, nil
}
