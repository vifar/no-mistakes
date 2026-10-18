package cli

import (
	"context"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/refreconcile"
	toon "github.com/toon-format/toon-go"
)

func refReconciliationMessages(ctx context.Context, d *db.DB, p *paths.Paths, repo *db.Repo) []string {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var messages []string
	for _, issue := range refreconcile.Inspect(ctx, d, p, repo) {
		messages = append(messages, issue.String())
	}
	return messages
}

func refReconciliationFields(cmdCtx context.Context, env *axiEnv) []toon.Field {
	if messages := refReconciliationMessages(cmdCtx, env.d, env.p, env.repo); len(messages) != 0 {
		return []toon.Field{{Key: "ref_reconciliation", Value: messages}}
	}
	return nil
}
