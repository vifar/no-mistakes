package steps

import (
	"context"
	"encoding/json"
	"html"
	"regexp"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const humanCoauthorFixture = "Co-Authored-By: Claude Smith <claude@example.com>"
const humanCoauthorDisclosure = "Disclosure: fixtures were generated with the repository recorder."

// These are published text contracts, not source checks. The owner's reported
// merge-bar expression is the independent oracle for the ambiguous raw field.
func assertPRHumanCoauthorEvidence(t *testing.T, body string) {
	t.Helper()
	bar := regexp.MustCompile(`(?i)Co-Authored-By:.*(Claude|Anthropic|OpenAI|Codex|ChatGPT|Grok|Copilot|noreply@anthropic\.com)`)
	if bar.MatchString(body) {
		t.Error("published evidence still resembles an attribution trailer")
	}
	for _, want := range []string{"preserved human co-author: Claude Smith", "claude@example.com", humanCoauthorDisclosure} {
		if !strings.Contains(body, want) {
			t.Errorf("published evidence lost %q", want)
		}
	}
}

func TestPRHumanCoauthorPresentation_BuildPRContent(t *testing.T) {
	t.Parallel()
	for _, provider := range []scm.Provider{scm.ProviderGitHub, scm.ProviderBitbucket} {
		t.Run(string(provider), func(t *testing.T) {
			dir, base, head := setupGitRepo(t)
			raw := "Preserve HUMAN co-author lines, including " + humanCoauthorFixture + ", and ordinary prose.\n\n" + humanCoauthorDisclosure
			ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
				payload, err := json.Marshal(prContent{Title: "fix(review): preserve repair rationale", Body: raw})
				return &agent.Result{Output: payload}, err
			}}
			sctx := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{})
			finding := findingsJSON(t, types.Findings{Items: []types.Finding{{ID: "R1", Severity: "warning", Description: humanCoauthorFixture + "\n\n" + humanCoauthorDisclosure}}})
			step := insertCompletedStep(t, sctx, types.StepReview, finding, "")
			insertCompletedStep(t, sctx, types.StepTest, findingsJSON(t, types.Findings{
				TestingSummary: "Human fixture retained.",
				Artifacts:      []types.TestArtifact{{Kind: "command-output", Label: "Generated PR findings", Content: humanCoauthorFixture + "\n\n" + humanCoauthorDisclosure}},
			}), "")
			content, err := (&PRStep{}).buildPRContent(sctx, "feature", "main", base, provider, 0)
			if err != nil {
				t.Fatal(err)
			}
			assertPRHumanCoauthorEvidence(t, content.Body)
			if strings.Count(content.Body, "preserved human co-author: Claude Smith") < 3 {
				t.Error("narrative, Review finding, and captured output must all retain the named human evidence")
			}
			rounds, err := sctx.DB.GetRoundsByStep(step.ID)
			if err != nil || len(rounds) != 1 || rounds[0].FindingsJSON == nil || *rounds[0].FindingsJSON != finding {
				t.Fatalf("presentation changed raw audit findings: %+v, %v", rounds, err)
			}
		})
	}
}

func TestPRHumanCoauthorPresentation_RefreshAndBudgets(t *testing.T) {
	t.Parallel()
	for _, provider := range []scm.Provider{scm.ProviderGitHub, scm.ProviderBitbucket} {
		for _, mode := range []string{config.PRAppendixFull, config.PRAppendixCollapsed} {
			t.Run(string(provider)+"/"+mode, func(t *testing.T) {
				sctx := &pipeline.StepContext{Config: &config.Config{}, UserIntent: "Keep " + humanCoauthorFixture}
				sctx.Config.PR.Appendix = mode
				narrative := "## What Changed\n\nKeep " + humanCoauthorFixture + ".\n\n" + humanCoauthorDisclosure
				evidence := "## Testing\n\n```text\nCo-Authored-By: Claude Smith &lt;claude@example.com&gt;\n```"
				pipelineMD := compliantPipelineBody(t, testPipelineHeadSHA)
				for _, body := range []string{
					assembleDraftPRBody(sctx, narrative, "Low risk", evidence, pipelineMD, 0, provider),
					buildPRBody(narrative, "Low risk", evidence, pipelineMD, sctx, provider),
				} {
					assertPRHumanCoauthorEvidence(t, body)
					if !strings.Contains(body, "preserved human co-author: Claude Smith &lt;claude@example.com&gt;") {
						t.Error("escaped captured evidence was lost or changed beyond its field label")
					}
				}
				const limit = 2200
				long := narrative + "\n\n" + strings.Repeat(humanCoauthorFixture+"\n", 100)
				for i, body := range []string{
					assembleDraftPRBody(sctx, long, "Low risk", evidence, pipelineMD, limit, provider),
					buildPRBodyWithin(long, "Low risk", evidence, pipelineMD, sctx, provider, limit),
				} {
					if (i == 0 && scm.PRBodyLen(body) > limit) || (i == 1 && len(body) > limit) {
						t.Error("expanded presentation exceeded the body budget")
					}
					if strings.Contains(body, "Co-Authored-By:") || !strings.Contains(body, testPipelineHeadSHA) {
						t.Error("budgeted presentation leaked a trailer or lost its attestation")
					}
				}
			})
		}
	}
}

func TestPRHumanCoauthorPresentation_OwnedAndRestamp(t *testing.T) {
	t.Parallel()
	for _, provider := range []scm.Provider{scm.ProviderGitHub, scm.ProviderBitbucket} {
		t.Run(string(provider), func(t *testing.T) {
			appendix := compliantPipelineBody(t, testPipelineHeadSHA) + "\n\n```text\n" + humanCoauthorFixture + "\n```\n\n" + humanCoauthorDisclosure
			content, err := composeOwnedPRContent(prOwnedBody{before: "author text"}, "fix: repair", appendix, 0)
			if err != nil {
				t.Fatal(err)
			}
			assertPRHumanCoauthorEvidence(t, content.Body)
			if _, err := parsePROwnedBody(content.Body); err != nil {
				t.Fatalf("presentation broke appendix integrity: %v", err)
			}
			for _, oldBody := range []string{appendix, "author text\n\n" + wrapPRAppendix(appendix) + "\nsuffix"} {
				updated, rebound, err := rebindOwnedPRAttestation(oldBody, strings.Repeat("b", 40), nil, pipelineAttestationPolicy{}, provider)
				if err != nil || !rebound {
					t.Fatalf("restamp failed: %t, %v", rebound, err)
				}
				assertPRHumanCoauthorEvidence(t, updated)
				if hasPRAppendixMarkers(updated) {
					if _, err := parsePROwnedBody(updated); err != nil || !strings.HasPrefix(updated, "author text") || !strings.HasSuffix(updated, "suffix") {
						t.Fatalf("restamp changed author text or invalidated integrity: %v", err)
					}
				}
			}
		})
	}
}

func TestPRHumanCoauthorPresentation_TemplateNarrative(t *testing.T) {
	t.Parallel()
	for _, provider := range []scm.Provider{scm.ProviderGitHub, scm.ProviderBitbucket} {
		t.Run(string(provider), func(t *testing.T) {
			sctx, ag, _ := templateTestContext(t)
			ag.runFn = func(context.Context, agent.RunOpts) (*agent.Result, error) {
				payload, err := json.Marshal(prContent{Title: "fix: repair", Body: filledPRTemplate + "\nKeep " + humanCoauthorFixture + "\n\n" + humanCoauthorDisclosure})
				return &agent.Result{Output: payload}, err
			}
			content, err := (&PRStep{}).buildPRContent(sctx, "feature", "main", sctx.Run.BaseSHA, provider, 0)
			if err != nil {
				t.Fatal(err)
			}
			assertPRHumanCoauthorEvidence(t, content.Body)
			if _, err := parsePROwnedBody(content.Body); err != nil {
				t.Fatalf("template presentation invalidated integrity: %v", err)
			}
		})
	}
}

func TestPRHumanCoauthorPresentation_ClassificationAndSourcePreservation(t *testing.T) {
	t.Parallel()
	for _, human := range []string{
		humanCoauthorFixture,
		"Co-Authored-By: Claude <claude@example.com>",
		`Co-Authored-By: "Claude Smith" <claude@example.com>`,
		"Co-Authored-By: private:writer <human@example.com>",
		"Co-Authored-By: Anthropic Engineer <person@example.com>",
	} {
		// Raw summaries/commits retain the actual human trailer, not the
		// presentation label. No configuration is needed to distinguish AI.
		if got := stripReviewAIAttribution(human, nil); got != human {
			t.Errorf("human source trailer changed: %q", got)
		}
		body := reviewFixCommitBody(human+"\n\n"+humanCoauthorDisclosure, `{"findings":[]}`, nil)
		if !strings.Contains(body, human) {
			t.Error("PR presentation changed the underlying commit body")
		}
		if got := presentPRCoauthors(body); strings.Contains(got, "Co-Authored-By:") || !strings.Contains(got, strings.TrimPrefix(human, "Co-Authored-By: ")) || !strings.Contains(got, humanCoauthorDisclosure) {
			t.Errorf("human identity/disclosure lost in presentation: %q", got)
		}
	}
	for _, ai := range []string{"gpt-5.5", "antigravity", "rovodev", "acp:cursor:model", `"Codex"`} {
		for _, address := range []string{" <model@example.com>", " &lt;model@example.com&gt;"} {
			got := presentPRCoauthors("Evidence: Co-Authored-By: " + ai + address + "\n\n" + humanCoauthorDisclosure)
			if got != "Evidence: \n\n"+humanCoauthorDisclosure {
				t.Errorf("AI evidence relabeled as human or disclosure changed: %q", got)
			}
		}
	}
	prose := "This documents the Co-Authored-By: field.\n\n" + humanCoauthorDisclosure
	if got := presentPRCoauthors(prose); got != prose {
		t.Errorf("ordinary prose changed: %q", got)
	}
}

func TestPRHumanCoauthorPresentation_FoldedEntries(t *testing.T) {
	t.Parallel()
	for _, address := range []string{"<claude@example.com>", "&lt;claude@example.com&gt;"} {
		input := "Co-Authored-By: Claude\n " + address + "\n\n" + humanCoauthorDisclosure
		want := "preserved human co-author: Claude\n " + address + "\n\n" + humanCoauthorDisclosure
		if got := presentPRCoauthors(input); got != want {
			t.Errorf("human evidence = %q, want %q", got, want)
		}
	}
	for _, address := range []string{"<noreply@anthropic.com>", "&lt;noreply@anthropic.com&gt;"} {
		input := "Co-Authored-By: Claude\n " + address + "\n\n" + humanCoauthorDisclosure
		if got := presentPRCoauthors(input); got != "\n\n"+humanCoauthorDisclosure {
			t.Errorf("folded AI evidence survived: %q", got)
		}
	}
}

func TestPRHumanCoauthorPresentation_WrappedFoldedEntries(t *testing.T) {
	t.Parallel()
	for _, wrapper := range []string{"> ", "**", "`", "- ", "\"", "'"} {
		for _, escaped := range []bool{false, true} {
			address := "<claude@example.com>"
			human := wrapper + "Co-Authored-By: Claude\n" + wrapper + address
			want := wrapper + "preserved human co-author: Claude\n" + wrapper + address
			if escaped {
				human = html.EscapeString(human)
				want = html.EscapeString(want)
			}
			if got := presentPRCoauthors(human); got != want {
				t.Errorf("human = %q, want %q", got, want)
			}
			ai := strings.ReplaceAll(human, "claude@example.com", "noreply@anthropic.com")
			if got := presentPRCoauthors(ai); strings.Contains(got, "noreply@anthropic.com") || strings.Contains(got, "co-author") || strings.Contains(got, "Co-Authored-By") {
				t.Errorf("AI attribution survived: %q", got)
			}
		}
	}
}
