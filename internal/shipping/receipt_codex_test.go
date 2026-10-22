package shipping

import (
	"context"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

type codexReceiptForge struct {
	*commentReceiptForge
	pending bool
}

func (h *codexReceiptForge) ReviewBotPending(context.Context, *scm.PR, scm.ReviewBot) (bool, error) {
	return h.pending, nil
}

// A previously recorded ready bit must never bypass live Codex threads or a
// pending review. Exclusions are explicit and apply only to the named bot.
func TestCIReadyReceiptCodexPolicyAndCompletion(t *testing.T) {
	for _, tc := range []struct {
		name, mode, external            string
		codex, greptile, pending, ready bool
	}{
		{name: "unresolved always", mode: "always", codex: true},
		{name: "resolved always", mode: "always", ready: true},
		{name: "unresolved on_failure", mode: "on_failure", codex: true, ready: true},
		{name: "pending always", mode: "always", pending: true},
		{name: "external pending Codex", mode: "always", pending: true, external: "Codex Review", ready: true},
		{name: "Greptile exclusion preserves Codex", mode: "always", external: "Greptile Review", codex: true},
		{name: "Codex exclusion preserves Greptile", mode: "always", external: "Codex Review", greptile: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newExportFixture(t)
			if err := f.database.SetRunCIReadyWithReason(f.run.ID, true, false); err != nil {
				t.Fatal(err)
			}
			f.forge.checks = []scm.Check{{Name: "build", Bucket: scm.CheckBucketPass}}
			comments := &commentReceiptForge{protectedReceiptForge: &protectedReceiptForge{receiptForge: f.forge, required: []string{"build"}}}
			if tc.codex {
				comments.comments = append(comments.comments, scm.ReviewComment{Author: "chatgpt-codex-connector[bot]", Path: "codex.go", Line: 73, Body: "P1 lost update"})
			}
			if tc.greptile {
				comments.comments = append(comments.comments, scm.ReviewComment{Author: "greptile-apps[bot]", Path: "greptile.go", Line: 1, Body: "unresolved concern"})
			}
			policy := scm.CheckScopePolicy{ReviewBotComments: tc.mode}
			if tc.external != "" {
				policy.ExternalChecks = []string{tc.external}
			}
			recordScope(t, f, policy)
			_, err := Export(context.Background(), f.database, f.repo, f.run.ID, "ci-ready", &codexReceiptForge{commentReceiptForge: comments, pending: tc.pending})
			if (err == nil) != tc.ready {
				t.Fatalf("receipt err=%v want ready=%v", err, tc.ready)
			}
		})
	}
}
