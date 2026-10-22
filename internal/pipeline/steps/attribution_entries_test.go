package steps

import (
	"context"
	"encoding/json"
	"html"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestAttributionEntryContract(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, input, kept, presented, forbidden string
		configured                              []string
	}{
		{name: "complete AI followed by quoted prose", input: "> Co-Authored-By: Codex <ai@example.com>\n> Focused checks passed", kept: "> Focused checks passed", presented: "\n> Focused checks passed", forbidden: "ai@example.com"},
		{name: "complete AI followed by another trailer key", input: "> Co-Authored-By: Codex <ai@example.com>\n> Verification: focused checks passed", kept: "> Verification: focused checks passed", presented: "\n> Verification: focused checks passed", forbidden: "ai@example.com"},
		{name: "folded AI followed by quoted prose", input: "> Co-Authored-By: Claude\n> <noreply@anthropic.com>\n> Focused checks passed", kept: "> Focused checks passed", presented: "\n> Focused checks passed", forbidden: "noreply@anthropic.com"},
		{name: "incomplete human followed by another trailer key", input: "> Co-Authored-By: Claude\n> Verification: focused checks passed", kept: "> Co-Authored-By: Claude\n> Verification: focused checks passed", presented: "> Co-Authored-By: Claude\n> Verification: focused checks passed"},
		{name: "folded human followed by quoted prose", input: "> Co-Authored-By: Claude\n> <claude@example.com>\n> Focused checks passed", kept: "> Co-Authored-By: Claude\n> <claude@example.com>\n> Focused checks passed", presented: "> preserved human co-author: Claude\n> <claude@example.com>\n> Focused checks passed"},
		{name: "generated followed by quoted prose", input: "> Generated with Codex\n> Focused checks passed", kept: "> Focused checks passed", presented: "\n> Focused checks passed", forbidden: "Generated with"},
		{name: "session followed by quoted prose", input: "> Codex-Session: abc\n> Focused checks passed", kept: "> Focused checks passed", presented: "\n> Focused checks passed", forbidden: "Codex-Session"},
		{name: "assistant followed by quoted prose", input: "> Assisted-by: Codex\n> Focused checks passed", kept: "> Focused checks passed", presented: "\n> Focused checks passed", forbidden: "Assisted-by"},
		{name: "bare human followed by quoted prose", input: "> Co-Authored-By: Claude Smith\n> Focused checks passed", kept: "> Co-Authored-By: Claude Smith\n> Focused checks passed", presented: "> Co-Authored-By: Claude Smith\n> Focused checks passed"},
		{name: "AI plain", input: "Co-Authored-By: Codex <ai@example.com>", forbidden: "ai@example.com"},
		{name: "AI quoted", input: "> Co-Authored-By: Codex <ai@example.com>", forbidden: "ai@example.com"},
		{name: "AI bold", input: "**Co-Authored-By: Codex <ai@example.com>**", forbidden: "ai@example.com"},
		{name: "assisted-by folded address", input: "Assisted-by: Codex\n <ai@example.com>", forbidden: "ai@example.com"},
		{name: "AI folded", input: "Co-Authored-By: Claude\n <noreply@anthropic.com>", forbidden: "noreply@anthropic.com"},
		{name: "AI folded quoted", input: "> Co-Authored-By: Claude\n> <noreply@anthropic.com>", forbidden: "noreply@anthropic.com"},
		{name: "model", input: "Co-Authored-By: gpt-5.5 <ai@example.com>", forbidden: "ai@example.com"},
		{name: "human", input: "Co-Authored-By: Claude <claude@example.com>", kept: "Co-Authored-By: Claude <claude@example.com>", presented: "preserved human co-author: Claude <claude@example.com>"},
		{name: "human folded quoted", input: "> Co-Authored-By: Claude Smith\n> <claude@example.com>", kept: "> Co-Authored-By: Claude Smith\n> <claude@example.com>", presented: "> preserved human co-author: Claude Smith\n> <claude@example.com>"},
		{name: "human folded", input: "Co-Authored-By: Claude\n <claude@example.com>", kept: "Co-Authored-By: Claude\n <claude@example.com>", presented: "preserved human co-author: Claude\n <claude@example.com>"},
		{name: "generated plain", input: "Generated with Codex", forbidden: "Generated with"},
		{name: "generated robot", input: "🤖 Generated with [Claude Code](https://claude.ai/code)", forbidden: "Generated with"},
		{name: "generated bold", input: "**Generated with Codex**", forbidden: "Generated with"},
		{name: "generated folded", input: "Generated with\n Codex", forbidden: "Generated with"},
		{name: "prose", input: "The Co-Authored-By: field is documented below.\n- See <https://example.com>.", kept: "The Co-Authored-By: field is documented below.\n- See <https://example.com>.", presented: "The Co-Authored-By: field is documented below.\n- See <https://example.com>."},
		{name: "recorder", input: "Generated with the repository recorder", kept: "Generated with the repository recorder", presented: "Generated with the repository recorder"},
		{name: "disclosure", input: "Disclosure: fixtures were generated with the repository recorder.\n\nKeep this paragraph.", kept: "Disclosure: fixtures were generated with the repository recorder.\n\nKeep this paragraph.", presented: "Disclosure: fixtures were generated with the repository recorder.\n\nKeep this paragraph."},
		{name: "configured bullet", input: "Build-Agent: private-identifier\n- Verification passed", kept: "- Verification passed", presented: "Build-Agent: private-identifier\n- Verification passed", forbidden: "private-identifier", configured: []string{"Build-Agent: private-identifier"}},
		{name: "generated dash bullet", input: "Generated with Codex\n- Verification passed", kept: "- Verification passed", presented: "\n- Verification passed", forbidden: "Generated with"},
		{name: "generated star bullet", input: "Generated with Codex\n* Verification passed", kept: "* Verification passed", presented: "\n* Verification passed", forbidden: "Generated with"},
		{name: "generated plus bullet", input: "Generated with Codex\n+ Verification passed", kept: "+ Verification passed", presented: "\n+ Verification passed", forbidden: "Generated with"},
		{name: "generated ordered bullet", input: "Generated with Codex\n1. Verification passed", kept: "1. Verification passed", presented: "\n1. Verification passed", forbidden: "Generated with"},
		{name: "generated quoted bullet", input: "Generated with Codex\n> - Verification passed", kept: "> - Verification passed", presented: "\n> - Verification passed", forbidden: "Generated with"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := stripReviewAIAttribution(tc.input, tc.configured); got != tc.kept {
				t.Fatalf("filtered = %q, want %q", got, tc.kept)
			}
			for _, escaped := range []bool{false, true} {
				input, want := tc.input, tc.presented
				if escaped {
					input, want = html.EscapeString(input), html.EscapeString(want)
				}
				if got := presentPRCoauthors(input); got != want {
					t.Errorf("presentation = %q, want %q", got, want)
				}
			}
			dir, base, head := setupGitRepo(t)
			output, err := json.Marshal(map[string]string{"summary": "Guard value\n" + tc.input, "verification": "passed"})
			if err != nil {
				t.Fatal(err)
			}
			ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
				writeAgentChange(t, dir)
				return &agent.Result{Output: output}, nil
			}}
			sctx := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{})
			sctx.Fixing = true
			sctx.Config.Commit.Trailers = tc.configured
			payload, err := json.Marshal(Findings{Items: []Finding{{ID: "r1", File: tc.input, Description: tc.input}}})
			if err != nil {
				t.Fatal(err)
			}
			sctx.PreviousFindings = string(payload)
			summary, err := executeFixMode(sctx, types.StepReview, fixExecutionOptions{FallbackSummary: "repair", RequireVerification: true})
			if err != nil {
				t.Fatal(err)
			}
			wantSummary := "Guard value"
			if tc.kept != "" {
				wantSummary += "\n" + tc.kept
			}
			if summary != wantSummary {
				t.Errorf("summary = %q, want %q", summary, wantSummary)
			}
			body := gitCmd(t, dir, "show", "-s", "--format=%b")
			if !strings.HasPrefix(body, wantSummary+"\n\nRound findings:") {
				t.Errorf("commit body = %q", body)
			}
			assertPersistedReviewFixSummary(t, sctx.DB, sctx.Run.ID, summary, wantSummary)
			for _, provider := range []scm.Provider{scm.ProviderGitHub, scm.ProviderBitbucket} {
				for _, current := range []bool{false, true} {
					findings, clean := string(payload), `{"findings":[]}`
					final := clean
					if current {
						final = findings
					}
					step := &db.StepResult{ID: "review", StepName: types.StepReview, Status: types.StepStatusCompleted, FindingsJSON: &final}
					rounds := map[string][]*db.StepRound{"review": {{Round: 1, Trigger: "initial", FindingsJSON: &findings}}}
					md, _ := buildPipelineSummaryFor([]*db.StepResult{step}, rounds, head, provider, pipelineAttestationPolicy{}, tc.configured...)
					decoded := html.UnescapeString(md)
					if tc.forbidden != "" && strings.Contains(decoded, tc.forbidden) {
						t.Errorf("published forbidden text: %s", md)
					}
					if tc.kept != "" {
						want := strings.TrimLeft(tc.presented, "\n")
						if len(tc.configured) > 0 {
							want = tc.kept
						}
						if !strings.Contains(decoded, want) {
							t.Errorf("published content lost %q: %s", want, md)
						}
					}
					if *step.FindingsJSON != final || *rounds["review"][0].FindingsJSON != string(payload) {
						t.Fatal("raw findings changed")
					}
				}
			}
		})
	}
}
