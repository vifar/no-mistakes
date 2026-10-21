package steps

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Issue #80's oracle is the message stored in a real commit, before any push.
func assertCorrectionMessage(t *testing.T, input, want string) {
	t.Helper()
	dir, base, head := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, &mockAgent{}, dir, base, head, config.Commands{})
	writeAgentChange(t, dir)
	gitCmd(t, dir, "add", "-A")
	if err := commitPipelineCorrection(sctx.Ctx, dir, input, sctx.Log); err != nil {
		t.Fatal(err)
	}
	if got := gitCmd(t, dir, "log", "-1", "--format=%B"); got != want {
		t.Fatalf("stored commit message = %q, want %q", got, want)
	}
}

func TestCommitAttributionClaudeCoauthor(t *testing.T) {
	t.Parallel()
	assertCorrectionMessage(t, "repair widget\n\nKeep the reason.\n\nCo-Authored-By: Claude <noreply@anthropic.com>", "repair widget\n\nKeep the reason.")
}
func TestCommitAttributionGeneratedWith(t *testing.T) {
	t.Parallel()
	assertCorrectionMessage(t, "repair widget\n\nKeep the reason.\n\nGenerated with Claude Code", "repair widget\n\nKeep the reason.")
}
func TestCommitAttributionClaudeSession(t *testing.T) {
	t.Parallel()
	assertCorrectionMessage(t, "repair widget\n\nKeep the reason.\n\nClaude-Session: abc", "repair widget\n\nKeep the reason.")
}
func TestCommitAttributionHumanCoauthor(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"Some Person <person@example.com>", "Claude Smith <claude@example.com>", "Claude <claude@example.com>", "Codex Jones <codex@example.com>"} {
		msg := "repair widget\n\nKeep the reason.\n\nCo-Authored-By: " + name
		assertCorrectionMessage(t, msg, msg)
	}
}
func TestCommitAttributionDisclosure(t *testing.T) {
	t.Parallel()
	msg := "repair widget\n\nDisclosure: fixtures were generated with the repository recorder.\n\nCo-Authored-By: Claude Smith <claude@example.com>"
	assertCorrectionMessage(t, msg, msg)
}
func TestCommitAttributionPreservesSubjectBodyAndHumanTrailers(t *testing.T) {
	t.Parallel()
	body := "repair widget\n\nKeep the reason.\nDisclosure: fixtures were generated with the repository recorder."
	human := "Co-Authored-By: Some Person <person@example.com>\nSigned-off-by: Victor I <vifarinde@gmail.com>"
	assertCorrectionMessage(t, body+"\n\nCo-Authored-By: Claude <noreply@anthropic.com>\n"+human+"\nCodex-Session: abc", body+"\n\n"+human)
}
func TestCommitAttributionAssistantAndCodexSession(t *testing.T) {
	t.Parallel()
	assertCorrectionMessage(t, "repair widget\n\nKeep the reason.\n\nAssisted-by: pi:local\nCodex-Session: abc", "repair widget\n\nKeep the reason.")
}
func TestCommitAttributionConfiguredTrailersAtCommitTime(t *testing.T) {
	t.Parallel()
	for _, step := range []types.StepName{types.StepReview, types.StepDocument, types.StepLint} {
		t.Run(string(step), func(t *testing.T) {
			dir, base, head := setupGitRepo(t)
			sctx := newTestContextWithDBRecords(t, &mockAgent{}, dir, base, head, config.Commands{})
			sctx.Config.Commit.Trailers = []string{"Assisted-by: pi:local", "Codex-Session: abc", "Co-Authored-By: Claude Smith <claude@example.com>"}
			writeAgentChange(t, dir)
			if _, err := commitAgentFixesWithResult(sctx, step, "repair widget", "fallback", &agent.Result{Provider: "pi", Model: "local"}); err != nil {
				t.Fatal(err)
			}
			if got := gitTrailers(t, dir); got != "" {
				t.Fatalf("trailers = %q", got)
			}
			if got := lastCommitMessage(t, dir); !strings.Contains(got, "repair widget") {
				t.Fatalf("subject lost: %q", got)
			}
		})
	}
}
func TestCommitAttributionPreservesBodyExamples(t *testing.T) {
	t.Parallel()
	msg := "Generated with Claude Code is a trailer\n\nExample of a forbidden trailer:\nCo-Authored-By: Claude <noreply@anthropic.com>\nThis paragraph is documentation.\n\nSigned-off-by: Some Person <person@example.com>"
	assertCorrectionMessage(t, msg, msg)
}
func TestCommitAttributionFoldedTrailers(t *testing.T) {
	t.Parallel()
	assertCorrectionMessage(t, "repair widget\n\nKeep the reason.\n\nCo-Authored-By: Claude\n <noreply@anthropic.com>\nAssisted-by: pi:local\n local model\nSigned-off-by: Some Person <person@example.com>", "repair widget\n\nKeep the reason.\n\nSigned-off-by: Some Person <person@example.com>")
}

func TestCommitAttributionSummaryBeforeSubjectNormalization(t *testing.T) {
	t.Parallel()
	dir, base, head := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, &mockAgent{}, dir, base, head, config.Commands{})
	writeAgentChange(t, dir)
	if err := commitAgentFixes(sctx, types.StepLint, "repair widget\n\nCo-Authored-By: Claude <noreply@anthropic.com>", "fallback"); err != nil {
		t.Fatal(err)
	}
	if got := lastCommitMessage(t, dir); got != "no-mistakes(lint): repair widget" {
		t.Fatalf("subject = %q", got)
	}
}

func TestCommitAttributionAgentSummaryThroughFixMode(t *testing.T) {
	t.Parallel()
	dir, base, head := setupGitRepo(t)
	raw, err := json.Marshal(map[string]string{"summary": "repair widget\n\nCo-Authored-By: Claude <noreply@anthropic.com>\nCodex-Session: abc"})
	if err != nil {
		t.Fatal(err)
	}
	ag := &mockAgent{runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		writeAgentChange(t, dir)
		return &agent.Result{Output: raw}, nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{})
	sctx.Fixing = true
	if _, err := executeFixMode(sctx, types.StepLint, fixExecutionOptions{FallbackSummary: "repair widget"}); err != nil {
		t.Fatal(err)
	}
	if got := lastCommitMessage(t, dir); got != "no-mistakes(lint): repair widget" {
		t.Fatalf("subject = %q", got)
	}
}
