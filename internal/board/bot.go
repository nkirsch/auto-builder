package board

import (
	"context"

	"github.com/nkirsch/auto-builder/internal/db"
)

// BotReply posts a reply as the bot (not human activity) and notifies.
func (b *Board) BotReply(ctx context.Context, it db.Item, body string) error {
	if _, err := b.Store.AddResponse(ctx, it.ID, b.BotLogin, body, false); err != nil {
		return err
	}
	b.Notified(ctx, it, b.BotLogin, body)
	return nil
}
