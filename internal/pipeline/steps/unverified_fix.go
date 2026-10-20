package steps

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// fixVerificationPassed is the only verification outcome that lets a
// verification-required fix round commit.
const fixVerificationPassed = "passed"

// verifiedFixSummarySchema is commitSummarySchema plus the fixer's own
// verification outcome. The outcome is a structured field rather than words
// read out of the summary: a summary is free text that legitimately names
// "blocked" or "unverified" things, and a fixer told its wording is policed
// can simply stop using those words.
var verifiedFixSummarySchema = json.RawMessage(fmt.Sprintf(`{
	"type": "object",
	"properties": {
		"summary": {"type": "string", "maxLength": %d},
		"verification": {"type": "string", "enum": ["passed", "blocked", "failed", "not_needed"]}
	},
	"required": ["summary", "verification"]
}`, config.MaxFixMessageSummaryBytes))

// UnverifiedFixError reports a fix round whose changes were discarded because
// the fixer did not report a passing verification. Nothing was committed, so
// the code it would have produced is never reviewed as a normal head.
type UnverifiedFixError struct {
	Step types.StepName
	// Verification is the outcome the fixer reported; empty when its output
	// carried none.
	Verification string
	Summary      string
}

func (e *UnverifiedFixError) Error() string {
	outcome := "no verification outcome"
	if e.Verification != "" {
		outcome = fmt.Sprintf("verification %q", e.Verification)
	}
	msg := fmt.Sprintf("%s fix round discarded: the fixer reported %s", e.Step, outcome)
	if e.Summary != "" {
		msg += fmt.Sprintf(" for %q", e.Summary)
	}
	return msg + "; nothing was committed. Make the repository's checks runnable in the pipeline worktree, then run again"
}

// fixVerification reads the verification outcome from a fixer's structured
// output. Missing or unparseable output reports no outcome, which never passes.
func fixVerification(result *agent.Result) string {
	if result == nil || result.Output == nil {
		return ""
	}
	var report struct {
		Verification string `json:"verification"`
	}
	if err := json.Unmarshal(result.Output, &report); err != nil {
		return ""
	}
	return strings.TrimSpace(report.Verification)
}

// refuseUnverifiedFix discards a fix round's changes, including any commits
// the fixer made itself, unless the fixer reported a passing verification. A
// round that changed nothing has nothing to discard and continues as before.
func refuseUnverifiedFix(sctx *pipeline.StepContext, stepName types.StepName, startingHead, summary string, result *agent.Result) error {
	verification := fixVerification(result)
	if verification == fixVerificationPassed {
		return nil
	}
	changed, err := discardUnverifiedFix(sctx, stepName, startingHead)
	if err != nil {
		return err
	}
	if !changed {
		return nil
	}
	return &UnverifiedFixError{Step: stepName, Verification: verification, Summary: summary}
}

func discardUnverifiedFix(sctx *pipeline.StepContext, stepName types.StepName, startingHead string) (bool, error) {
	ctx := context.WithoutCancel(sctx.Ctx)
	head, err := git.HeadSHA(ctx, sctx.WorkDir)
	if err != nil {
		return false, fmt.Errorf("resolve head after %s fix: %w", stepName, err)
	}
	status, err := git.Run(ctx, sctx.WorkDir, "status", "--porcelain")
	if err != nil {
		return false, fmt.Errorf("check %s changes: %w", stepName, err)
	}
	if head == startingHead && strings.TrimSpace(status) == "" {
		return false, nil
	}
	if _, err := git.Run(ctx, sctx.WorkDir, "reset", "--hard", startingHead); err != nil {
		return false, fmt.Errorf("discard unverified %s fix: %w", stepName, err)
	}
	if _, err := git.Run(ctx, sctx.WorkDir, "clean", "-fd"); err != nil {
		return false, fmt.Errorf("discard unverified %s fix: %w", stepName, err)
	}
	return true, nil
}
