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
	for _, want := range []string{"co-author (quoted): Claude Smith", "claude@example.com", humanCoauthorDisclosure} {
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
			raw := "Preserve HUMAN co-author lines, including:\n" + humanCoauthorFixture + "\nAnd ordinary prose.\n\n" + humanCoauthorDisclosure
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
			if strings.Count(content.Body, "co-author (quoted): Claude Smith") < 3 {
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
				sctx := &pipeline.StepContext{Config: &config.Config{}, UserIntent: "Keep:\n" + humanCoauthorFixture}
				sctx.Config.PR.Appendix = mode
				narrative := "## What Changed\n\nKeep:\n" + humanCoauthorFixture + "\n\n" + humanCoauthorDisclosure
				evidence := "## Testing\n\n```text\n" + html.EscapeString(prDisplayText(humanCoauthorFixture)) + "\n```"
				pipelineMD := compliantPipelineBody(t, testPipelineHeadSHA)
				for _, body := range []string{
					assembleDraftPRBody(sctx, narrative, "Low risk", evidence, pipelineMD, 0, provider),
					buildPRBody(narrative, "Low risk", evidence, pipelineMD, sctx, provider),
				} {
					assertPRHumanCoauthorEvidence(t, body)
					if !strings.Contains(body, "co-author (quoted): Claude Smith &lt;claude@example.com&gt;") {
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
			appendix := compliantPipelineBody(t, testPipelineHeadSHA) + "\n\n```text\n" + prDisplayText(humanCoauthorFixture) + "\n```\n\n" + humanCoauthorDisclosure
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
				payload, err := json.Marshal(prContent{Title: "fix: repair", Body: filledPRTemplate + "\nKeep:\n" + humanCoauthorFixture + "\n\n" + humanCoauthorDisclosure})
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
		if got := prDisplayText(body); strings.Contains(got, "Co-Authored-By:") || !strings.Contains(got, strings.TrimPrefix(human, "Co-Authored-By: ")) || !strings.Contains(got, humanCoauthorDisclosure) {
			t.Errorf("human identity/disclosure lost in presentation: %q", got)
		}
	}
	for _, ai := range []string{"gpt-5.5", "antigravity", "rovodev", "acp:cursor:model", `"Codex"`} {
		for _, address := range []string{" <model@example.com>", " &lt;model@example.com&gt;"} {
			got := prDisplayText("Evidence:\nCo-Authored-By: " + ai + address + "\n\n" + humanCoauthorDisclosure)
			if got != "Evidence:\n\n\n"+humanCoauthorDisclosure {
				t.Errorf("AI evidence relabeled as human or disclosure changed: %q", got)
			}
		}
	}
	prose := "This documents the Co-Authored-By: field.\n\n" + humanCoauthorDisclosure
	if got := prDisplayText(prose); got != prose {
		t.Errorf("ordinary prose changed: %q", got)
	}
}

func TestPRHumanCoauthorPresentation_FoldedEntries(t *testing.T) {
	t.Parallel()
	for _, address := range []string{"<claude@example.com>", "&lt;claude@example.com&gt;"} {
		input := "Co-Authored-By: Claude\n " + address + "\n\n" + humanCoauthorDisclosure
		want := input
		if got := prDisplayText(input); got != want {
			t.Errorf("human evidence = %q, want %q", got, want)
		}
	}
	for _, address := range []string{"<noreply@anthropic.com>", "&lt;noreply@anthropic.com&gt;"} {
		input := "Co-Authored-By: Claude\n " + address + "\n\n" + humanCoauthorDisclosure
		if got := prDisplayText(input); got != input {
			t.Errorf("line-local mention changed: %q", got)
		}
	}
}

func TestPRHumanCoauthorPresentation_WrappedFoldedEntries(t *testing.T) {
	t.Parallel()
	for _, wrappers := range [][2]string{{"> ", ""}, {"** ", "**"}, {"` ", "`"}, {"\" ", "\""}, {"' ", "'"}} {
		wrapper, close := wrappers[0], wrappers[1]
		for _, escaped := range []bool{false, true} {
			address := "<claude@example.com>"
			human := wrapper + "Co-Authored-By: Claude" + close + "\n" + wrapper + address + close
			want := human
			if escaped {
				human = html.EscapeString(human)
				want = html.EscapeString(want)
			}
			if got := prDisplayText(human); got != want {
				t.Errorf("human = %q, want %q", got, want)
			}
			ai := strings.ReplaceAll(human, "claude@example.com", "noreply@anthropic.com")
			if got := prDisplayText(ai); got != ai {
				t.Errorf("line-local mention changed: %q", got)
			}
		}
	}
}

func TestPRHumanCoauthorPresentation_EncodedKeyIsNotAReplacementTarget(t *testing.T) {
	t.Parallel()
	for _, text := range []string{
		"&#67;o-Authored-By: Claude <claude@example.com>",
		"Co-Authored-By&#58; Human: Writer <human@example.com>",
	} {
		if got := prDisplayText(text); got != text {
			t.Errorf("encoded prose changed: %q, want %q", got, text)
		}
	}
}

func TestPRConfiguredAttributionHTMLForms(t *testing.T) {
	t.Parallel()
	for _, configured := range []string{"Build-Agent: private&#45;identifier", "Build-Agent: private-identifier", "Build&#45;Agent: private-identifier"} {
		decoded := html.UnescapeString(configured)
		for _, input := range []string{configured, decoded, html.EscapeString(decoded), "**" + configured + "**", "> " + configured} {
			if got := prDisplayText(input, configured); got != "" {
				t.Errorf("display retained exact configured form %q: %q", input, got)
			}
		}
		for _, input := range []string{configured, decoded} {
			if got := stripReviewAIAttribution(input, []string{configured}); got != "" {
				t.Errorf("Review retained exact configured form %q: %q", input, got)
			}
		}
		for _, input := range []string{configured + "-", decoded + ".", html.EscapeString(configured) + "-"} {
			if got := prDisplayText(input, configured); got != input {
				t.Errorf("lookalike changed: %q, want %q", got, input)
			}
		}
	}
	for _, attribution := range []string{"&#67;o-Authored-By: &#67;odex &lt;ai@example.com&gt;", "Generated with &#67;odex"} {
		if got := prDisplayText(attribution); got != "" {
			t.Errorf("encoded native attribution survived: %q", got)
		}
	}
}

func TestPRConfiguredAttributionCaseSensitive(t *testing.T) {
	t.Parallel()
	for _, configured := range []string{"Build-Agent: Worker-A", "Build-Agent: Worker&#45;A"} {
		for _, input := range []string{"Build-Agent: Worker-A", "Build-Agent: Worker&#45;A", "**Build-Agent: Worker-A**"} {
			if got := prDisplayText(input, configured); got != "" {
				t.Errorf("exact configured attribution survived: %q", got)
			}
			if got := stripReviewAIAttribution(input, []string{configured}); got != "" {
				t.Errorf("Review exact configured attribution survived: %q", got)
			}
		}
		for _, input := range []string{"Build-Agent: worker-a", "Build-Agent: worker&#45;a"} {
			if got := prDisplayText(input, configured); got != input {
				t.Errorf("case-distinct evidence changed: %q, want %q", got, input)
			}
			if got := stripReviewAIAttribution(input, []string{configured}); got != input {
				t.Errorf("Review case-distinct evidence changed: %q, want %q", got, input)
			}
		}
		// Git compares a trailer KEY case-insensitively and allows whitespace
		// before the colon, so the same configured trailer cannot evade the
		// filter by changing the key's casing or spacing.
		for _, input := range []string{"build-agent: Worker-A", "BUILD-AGENT : Worker-A", "Build-Agent\t:\tWorker-A"} {
			if got := prDisplayText(input, configured); got != "" {
				t.Errorf("exact configured attribution survived as %q", got)
			}
			if got := stripReviewAIAttribution(input, []string{configured}); got != "" {
				t.Errorf("Review exact configured attribution survived as %q", got)
			}
		}
	}
	for _, input := range []string{"cO-aUTHORED-bY: cOdEx <ai@example.com>", "gEnErAtEd WiTh cOdEx", "gEnErAtEd WiTh &#67;oDeX"} {
		if got := prDisplayText(input, "Build-Agent: Worker-A"); got != "" {
			t.Errorf("native attribution survived: %q", got)
		}
	}
	if got := prDisplayText(humanCoauthorFixture, "Build-Agent: Worker-A"); got != "co-author (quoted): Claude Smith <claude@example.com>" {
		t.Errorf("human evidence changed: %q", got)
	}
}

func TestPRConfiguredAttributionSingleDecode(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ configured, input, want string }{
		{"Build-Agent: worker&amp;copy;", "Build-Agent: worker&amp;copy;", ""},
		{"Build-Agent: worker&copy;", "Build-Agent: worker&amp;copy;", "Build-Agent: worker&amp;copy;"},
		{"Build-Agent: worker&amp;copy;", "Build-Agent: worker&copy;", "Build-Agent: worker&copy;"},
		{"Build-Agent: worker", "Build-Agent: worker", ""},
		{"Build-Agent: worker-A", "Build-Agent: worker&#45;A", ""},
		{"Build-Agent: worker-A", "Build-Agent: worker-A", ""},
	} {
		if got := prDisplayText(tc.input, tc.configured); got != tc.want {
			t.Errorf("display(%q, %q) = %q, want %q", tc.input, tc.configured, got, tc.want)
		}
		if got := stripReviewAIAttribution(tc.input, []string{tc.configured}); got != tc.want {
			t.Errorf("Review(%q, %q) = %q, want %q", tc.input, tc.configured, got, tc.want)
		}
	}
}
