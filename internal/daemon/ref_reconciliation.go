package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/logstore"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/refreconcile"
)

// Called under the daemon singleton lock, before any recovery can launch work.
// Reporting is deliberately independent of SQLite writes: the missing receive
// could itself be the result of a writer lock or a crash before durable rename.
func reconcileRefsOnStartup(d *db.DB, p *paths.Paths) error {
	repos, err := d.GetRepos()
	var issues []refreconcile.Issue
	if err != nil {
		issues = append(issues, refreconcile.Issue{Code: refreconcile.Failed, Detail: fmt.Sprintf("read managed repositories: %v", err)})
	}
	for _, repo := range repos {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		issues = append(issues, refreconcile.Inspect(ctx, d, p, repo)...)
		cancel()
	}
	if len(issues) == 0 {
		return nil
	}
	eventPath := filepath.Join(p.Root(), "logs", "ref-reconciliation.jsonl")
	events, err := logstore.Open(eventPath, logstore.LifecyclePolicy())
	if err != nil {
		return fmt.Errorf("%s: open ref reconciliation event log: %w", refreconcile.Failed, err)
	}
	defer events.Close()
	for _, issue := range issues {
		slog.Warn(issue.Code, "repo", issue.RepoID, "ref", issue.Ref, "head", issue.Head, "detail", issue.Detail)
		event := struct {
			At time.Time `json:"at"`
			refreconcile.Issue
		}{time.Now().UTC(), issue}
		raw, err := json.Marshal(event)
		if err != nil {
			return fmt.Errorf("%s: encode ref reconciliation event: %w", refreconcile.Failed, err)
		}
		if _, err := events.Write(append(raw, '\n')); err != nil {
			return fmt.Errorf("%s: write ref reconciliation event: %w", refreconcile.Failed, err)
		}
	}
	if err := events.Close(); err != nil {
		return fmt.Errorf("%s: close ref reconciliation event log: %w", refreconcile.Failed, err)
	}
	f, err := os.OpenFile(eventPath, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("%s: open ref reconciliation event log for sync: %w", refreconcile.Failed, err)
	}
	defer f.Close()
	if err := f.Sync(); err != nil {
		return fmt.Errorf("%s: sync ref reconciliation event log: %w", refreconcile.Failed, err)
	}
	return nil
}
