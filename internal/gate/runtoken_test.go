package gate

import (
	"context"
	"errors"
	"testing"

	"github.com/nkirsch/auto-builder/internal/db"
)

type envRecorder struct{ env map[string]string }

func (e *envRecorder) LaunchEnv(_ context.Context, env map[string]string) (string, error) {
	e.env = env
	return "exec-1", nil
}

func TestTokenLauncher(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	rec := &envRecorder{}
	g := &Gate{Store: st, Launcher: TokenLauncher{Store: st, Env: rec}}
	id, err := g.launch(ctx, "now", "nkirsch")
	if err != nil {
		t.Fatal(err)
	}
	token := rec.env["RUN_TOKEN"]
	if rec.env["RUN_ID"] == "" || len(token) != 64 {
		t.Fatalf("env: %v", rec.env)
	}
	if got, err := st.RunByTokenHash(ctx, HashRunToken(token)); err != nil || got != id {
		t.Fatalf("lookup: %d %v", got, err)
	}
	if err := g.Cancel(ctx, id, "nkirsch"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RunByTokenHash(ctx, HashRunToken(token)); !errors.Is(err, db.ErrNotFound) {
		t.Fatalf("after cancel: %v", err)
	}
}
