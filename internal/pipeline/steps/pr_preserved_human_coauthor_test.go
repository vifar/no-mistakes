package steps

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// The owner's merge bar reads the published pull-request body with a literal
// expression. These are the strings the change is about: the human co-author
// the fix must PRESERVE, the AI-named co-author it must strip, and the
// disclosure it must keep.
var mergeBarAIAttribution = regexp.MustCompile(`(?i)Co-Authored-By:.*(Claude|Anthropic|OpenAI|Codex|ChatGPT|Grok|Copilot|noreply@anthropic\.com)`)

const (
	preservedHumanCoauthor = "Co-Authored-By: Claude Smith <claude@example.com>"
	aiNamedCoauthor        = "Co-Authored-By: gpt-5.5 <model@example.com>"
	requiredDisclosure     = "Disclosure: fixtures were generated with the repository recorder."
	ordinaryProse          = "The Build-Agent value is mentioned in ordinary prose."
)

// recordedPayload is the shape a recorded findings payload has in the
// published body: one line whose line breaks are the two-character escape, so
// its entries are one line to a line-based scanner.
func recordedPayload(t *testing.T) string {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"findings": []any{map[string]any{
			"id":          "r1",
			"description": "Guard unsafe value\n" + preservedHumanCoauthor + "\n" + aiNamedCoauthor + "\n" + requiredDisclosure + "\n" + ordinaryProse,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(payload)
}

// The published body is the publication. A preserved human co-author is
// rendered as a labelled field, so the body carries no line that begins with
// "Co-Authored-By:" and no text check can mistake it for AI attribution. The
// co-author itself stays visible, and the AI-named one is gone entirely.
func TestPRPreservedHumanCoauthor_RenderedBody(t *testing.T) {
	t.Parallel()
	for _, provider := range []scm.Provider{scm.ProviderGitHub, scm.ProviderBitbucket} {
		t.Run(string(provider), func(t *testing.T) {
			t.Parallel()
			dir, base, head := setupGitRepo(t)
			narrative := "Preserve the human co-author line.\n\n" + preservedHumanCoauthor + "\n" + ordinaryProse + "\n\n" + requiredDisclosure
			ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
				payload, err := json.Marshal(prContent{Title: "fix(review): preserve repair rationale", Body: narrative})
				return &agent.Result{Output: payload}, err
			}}
			sctx := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{})

			// The Review detail, the recorded evidence and a recorded payload
			// each carry the same fixture, and each is a different owner of
			// published text.
			insertCompletedStep(t, sctx, types.StepReview, findingsJSON(t, types.Findings{
				Items: []types.Finding{{ID: "R1", Severity: "warning", Description: preservedHumanCoauthor + "\n" + aiNamedCoauthor + "\n\n" + requiredDisclosure}},
			}), "")
			insertCompletedStep(t, sctx, types.StepTest, findingsJSON(t, types.Findings{
				TestingSummary: "Focused checks passed.",
				Artifacts: []types.TestArtifact{
					{Kind: "command-output", Label: "Real Git commit bodies", Content: preservedHumanCoauthor + "\n" + aiNamedCoauthor + "\n\n" + requiredDisclosure + "\n" + ordinaryProse},
					{Kind: "log", Label: "Persisted summaries and raw audit findings", Content: recordedPayload(t)},
				},
			}), "")

			content, err := (&PRStep{}).buildPRContent(sctx, "feature", "main", base, provider, 0)
			if err != nil {
				t.Fatal(err)
			}
			body := content.Body

			for _, line := range strings.Split(body, "\n") {
				if strings.HasPrefix(strings.TrimSpace(line), "Co-Authored-By:") {
					t.Errorf("published body has a raw co-author trailer line: %q", line)
				}
			}
			if mergeBarAIAttribution.MatchString(body) {
				t.Error("published body still resembles an AI attribution trailer")
			}
			if !strings.Contains(body, "co-author (quoted): Claude Smith <claude@example.com>") {
				t.Error("published body lost the preserved human co-author")
			}
			if !strings.Contains(body, "claude@example.com") {
				t.Error("published body lost the preserved human co-author address")
			}
			if strings.Contains(body, "gpt-5.5") {
				t.Error("published body kept the AI-named co-author")
			}
			if !strings.Contains(body, requiredDisclosure) {
				t.Error("published body lost the required disclosure")
			}
			if !strings.Contains(body, ordinaryProse) {
				t.Error("published body rewrote ordinary prose")
			}
			if !strings.Contains(body, "Guard unsafe value") {
				t.Error("published body lost the recorded payload content")
			}
		})
	}
}

// The filter is unchanged. The commit keeps the preserved human co-author byte
// for byte and still strips the AI-named one: only the published body is
// rendered as a labelled field.
func TestPRPreservedHumanCoauthor_CommitFilterUnchanged(t *testing.T) {
	t.Parallel()
	dir, base, head := setupGitRepo(t)
	summary := "Guard receipts\n\n" + preservedHumanCoauthor + "\n" + aiNamedCoauthor + "\n" + requiredDisclosure + "\n" + ordinaryProse
	output, err := json.Marshal(map[string]string{"summary": summary, "verification": "passed"})
	if err != nil {
		t.Fatal(err)
	}
	ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		writeAgentChange(t, dir)
		return &agent.Result{Output: output}, nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{})
	sctx.Fixing = true
	sctx.PreviousFindings = `{"findings":[{"id":"r1","file":"receipt.go","description":"Stale receipt could approve a different head"}]}`
	if _, err := executeFixMode(sctx, types.StepReview, fixExecutionOptions{FallbackSummary: "address review findings", RequireVerification: true}); err != nil {
		t.Fatal(err)
	}
	body := gitCmd(t, dir, "show", "-s", "--format=%b", "HEAD")
	if !strings.Contains(body, preservedHumanCoauthor) {
		t.Errorf("commit body lost the preserved human co-author byte for byte: %q", body)
	}
	if strings.Contains(body, "gpt-5.5") {
		t.Errorf("commit body kept the AI-named co-author: %q", body)
	}
	if !strings.Contains(body, requiredDisclosure) {
		t.Errorf("commit body lost the required disclosure: %q", body)
	}
	if !strings.Contains(body, ordinaryProse) {
		t.Errorf("commit body rewrote ordinary prose: %q", body)
	}
	if !strings.Contains(body, "- receipt.go: Stale receipt could approve a different head") {
		t.Errorf("commit body lost the round findings list: %q", body)
	}
}

// A round with no summary still records the unavailable notice, and the
// subject is still the normalized summary.
func TestPRPreservedHumanCoauthor_CommitContractUnchanged(t *testing.T) {
	t.Parallel()
	dir, base, head := setupGitRepo(t)
	output, err := json.Marshal(map[string]string{"summary": "", "verification": "passed"})
	if err != nil {
		t.Fatal(err)
	}
	ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		writeAgentChange(t, dir)
		return &agent.Result{Output: output}, nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{})
	sctx.Fixing = true
	sctx.PreviousFindings = `{"findings":[{"id":"r1","file":"receipt.go","description":"Stale receipt could approve a different head"}]}`
	if _, err := executeFixMode(sctx, types.StepReview, fixExecutionOptions{FallbackSummary: "address review findings", RequireVerification: true}); err != nil {
		t.Fatal(err)
	}
	body := gitCmd(t, dir, "show", "-s", "--format=%b", "HEAD")
	if !strings.Contains(body, "body: unavailable - fixer returned no summary") {
		t.Errorf("commit body lost the unavailable notice: %q", body)
	}
	if got := lastCommitMessage(t, dir); got != "no-mistakes(review): address review findings" {
		t.Errorf("subject = %q", got)
	}
}
