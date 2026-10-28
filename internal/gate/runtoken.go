package gate

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"strconv"

	"github.com/nkirsch/auto-builder/internal/db"
)

// HashRunToken is how a runner token is stored and looked up.
func HashRunToken(token string) []byte {
	h := sha256.Sum256([]byte(token))
	return h[:]
}

// TokenLauncher gives each run its own RUN_TOKEN (only its hash is stored),
// valid while the run is requested or running, so the agent never holds a
// credential that outlives its run.
type TokenLauncher struct {
	Store *db.Store
	Env   EnvLauncher
}

func (l TokenLauncher) Launch(ctx context.Context, runID int) (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	token := hex.EncodeToString(b)
	if err := l.Store.SetRunTokenHash(ctx, runID, HashRunToken(token)); err != nil {
		return "", err
	}
	return l.Env.LaunchEnv(ctx, map[string]string{"RUN_ID": strconv.Itoa(runID), "RUN_TOKEN": token})
}

func (l TokenLauncher) Cancel(ctx context.Context, runID int, execution string) error {
	if c, ok := l.Env.(Canceler); ok {
		return c.Cancel(ctx, runID, execution)
	}
	return nil
}
