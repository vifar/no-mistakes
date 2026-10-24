package steps

import (
	"context"
	"html"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestInlineCoAuthorPublicationPaths(t *testing.T) {
	const prefix = "Low: handle ``literal ` tick``; "
	const payload = `{ "summary" : "Recorded risk Co-Authored-By: Codex \u003cbot@example.invalid\u003e", "escape": "A\u0042\/C", "number": 1.200e+3 }`
	root := t.TempDir()
	path := filepath.Join(root, "capture.json")
	if err := os.WriteFile(path, []byte(payload), 0600); err != nil {
		t.Fatal(err)
	}
	captured, ok := embeddedArtifactText(types.TestArtifact{Path: path}, testingSummaryOptions{evidenceRoot: root}, &testingArtifactRenderState{remainingEmbeddedBytes: maxEmbeddedArtifactsTotalBytes})
	if !ok {
		t.Fatal("captured evidence omitted")
	}
	for _, provider := range []scm.Provider{scm.ProviderGitHub, scm.ProviderBitbucket} {
		for _, mode := range []string{config.PRAppendixFull, config.PRAppendixMinimal, config.PRAppendixCollapsed} {
			t.Run(string(provider)+"/"+mode, func(t *testing.T) {
				ctx := &pipeline.StepContext{Config: &config.Config{}}
				ctx.Config.PR.Appendix = mode
				for name, raw := range map[string]string{
					"backticks": prefix + "Co-Authored-By: Codex <bot@example.invalid>",
					"payload":   captured,
				} {
					for assembly, body := range map[string]string{
						"assemble": assemblePRBody(ctx, "## What Changed\n\n"+raw, raw, "## Testing\n\n"+raw, "## Pipeline\n\n"+raw, 0, provider),
						"build":    buildPRBodyWithin("## What Changed\n\n"+raw, raw, "## Testing\n\n"+raw, "## Pipeline\n\n"+raw, ctx, provider, maxPullRequestBodyBytes),
						"evidence": appendixEvidence(mode, prBodyFlavorFor(provider), raw, "## Testing\n\n"+raw, "## Pipeline\n\n"+raw, provider),
					} {
						t.Run(name+"/"+assembly, func(t *testing.T) {
							view := html.UnescapeString(body)
							narrativeOnly := mode == config.PRAppendixMinimal && name == "backticks" && assembly != "evidence"
							if strings.Contains(view, "Co-Authored-By:") || strings.Contains(view, "Codex") || strings.Contains(view, "bot@example.invalid") || strings.Contains(view, prAIAttributionNotice) == narrativeOnly {
								t.Fatalf("publication retained an inline AI claim or used the wrong display mode: %q", body)
							}
							if name == "backticks" && !strings.Contains(view, strings.TrimSpace(prefix)) {
								t.Fatalf("publication changed the completed code span: %q", body)
							}
							if name == "payload" && !strings.Contains(view, `"escape": "A\u0042\/C", "number": 1.200e+3`) {
								t.Fatalf("publication changed unrelated recorded bytes: %q", body)
							}
						})
					}
				}
			})
		}
	}
}

func TestCapturedTextCoAuthorInFencedTestingPublication(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "capture.txt")
	const raw = "Recorded risk Co-Authored-By: Codex <bot@example.invalid>\nRecorded helper Co-Authored-By: Ada <ada@example.com>\n"
	if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	artifact := types.TestArtifact{Kind: "command-output", Label: "Captured command", Path: path}
	findings := findingsJSON(t, types.Findings{Artifacts: []types.TestArtifact{artifact}})
	steps := []*db.StepResult{{ID: "test", StepName: types.StepTest, Status: types.StepStatusCompleted, FindingsJSON: &findings}}
	for _, provider := range []scm.Provider{scm.ProviderGitHub, scm.ProviderBitbucket} {
		t.Run(string(provider), func(t *testing.T) {
			opts := testingSummaryOptions{flavor: prBodyFlavorFor(provider), repoRoot: root, evidenceRoot: root}
			if text, ok := embeddedArtifactText(artifact, opts, &testingArtifactRenderState{remainingEmbeddedBytes: maxEmbeddedArtifactsTotalBytes}); !ok || !strings.Contains(text, "Recorded risk") {
				t.Fatal("captured text evidence omitted")
			}
			testingMD := buildTestingSummary(steps, nil, opts)
			ctx := &pipeline.StepContext{Config: &config.Config{}}
			ctx.Config.PR.Appendix = config.PRAppendixFull
			for name, body := range map[string]string{
				"testing":  testingMD,
				"assemble": assemblePRBody(ctx, "## What Changed\n\nKeep captured evidence.", "Low risk", testingMD, "", 0, provider),
				"build":    buildPRBodyWithin("## What Changed\n\nKeep captured evidence.", "Low risk", testingMD, "", ctx, provider, maxPullRequestBodyBytes),
			} {
				t.Run(name, func(t *testing.T) {
					view := html.UnescapeString(body)
					if strings.Contains(view, "Co-Authored-By:") || strings.Contains(view, "Codex") || strings.Contains(view, "bot@example.invalid") {
						t.Fatalf("fenced captured text published an AI claim: %q", body)
					}
					for _, want := range []string{"```text", "Recorded risk " + prAIAttributionNotice, "Recorded helper co-author (quoted): Ada <ada@example.com>"} {
						if !strings.Contains(view, want) {
							t.Errorf("fenced captured text lost %q: %q", want, body)
						}
					}
				})
			}
		})
	}
}

func TestInlineCodeSpansExcludeFenceDelimiters(t *testing.T) {
	for _, line := range []string{"```text `literal`", "   ````text `literal`", "~~~text `literal`", "   ~~~~text `literal`"} {
		if spans := inlineCodeSpans(line); len(spans) != 0 {
			t.Errorf("fence delimiter %q produced inline spans: %v", line, spans)
		}
	}
}

func TestRenderInlineCoAuthorKeys(t *testing.T) {
	for _, tt := range []struct {
		name string
		text string
		want string
	}{
		{"AI claim", "Recorded risk Co-Authored-By: Codex <bot@example.invalid>", "Recorded risk " + prAIAttributionNotice},
		{"AI sentence", "Recorded risk Co-Authored-By: Codex <bot@example.invalid>.", "Recorded risk " + prAIAttributionNotice + "."},
		{"escaped AI claim", "Recorded risk Co-Authored-By: Codex &lt;bot@example.invalid&gt;.", "Recorded risk " + prAIAttributionNotice + "."},
		{"human sentence", "Recorded risk Co-Authored-By: Ada <ada@example.com>.", "Recorded risk co-author (quoted): Ada <ada@example.com>."},
		{"escaped human sentence", "Recorded risk Co-Authored-By: Ada &lt;ada@example.com&gt;.", "Recorded risk co-author (quoted): Ada &lt;ada@example.com&gt;."},
		{"closing punctuation", "Recorded risk (Co-Authored-By: Ada <ada@example.com>);", "Recorded risk (co-author (quoted): Ada <ada@example.com>);"},
		{"quoted field", `The field "Co-Authored-By:" identifies Ada <ada@example.com>`, `The field "Co-Authored-By:" identifies Ada <ada@example.com>`},
		{"single quoted field", "The field 'Co-Authored-By:' identifies Ada <ada@example.com>", "The field 'Co-Authored-By:' identifies Ada <ada@example.com>"},
		{"curly quoted field", "The field “Co-Authored-By:” identifies Ada <ada@example.com>", "The field “Co-Authored-By:” identifies Ada <ada@example.com>"},
		{"escaped quoted field", "The field &quot;Co-Authored-By:&quot; identifies Ada &lt;ada@example.com&gt;", "The field &quot;Co-Authored-By:&quot; identifies Ada &lt;ada@example.com&gt;"},
		{"code span", "Recorded `Co-Authored-By: Codex <bot@example.invalid>`", "Recorded `Co-Authored-By: Codex <bot@example.invalid>`"},
		{"completed double backticks", "Low: handle ``literal ` tick``; Co-Authored-By: Codex <bot@example.invalid>", "Low: handle ``literal ` tick``; " + prAIAttributionNotice},
		{"escaped backtick", "Low: handle \\`; Co-Authored-By: Codex <bot@example.invalid>", "Low: handle \\`; " + prAIAttributionNotice},
		{"unmatched backtick", "Low: handle `; Co-Authored-By: Codex <bot@example.invalid>", "Low: handle `; " + prAIAttributionNotice},
		{"double backtick span", "Recorded ``literal ` Co-Authored-By: Codex <bot@example.invalid>``", "Recorded ``literal ` Co-Authored-By: Codex <bot@example.invalid>``"},
		{"cross-line backticks", "Recorded ``literal\nCo-Authored-By: Codex <bot@example.invalid>\n``", "Recorded ``literal\n" + prAIAttributionNotice + "\n``"},
		{"backtick fence", "```text\nRecorded risk Co-Authored-By: Codex <bot@example.invalid>\n```", "```text\nRecorded risk " + prAIAttributionNotice + "\n```"},
		{"tilde fence", "   ~~~~text\nRecorded risk Co-Authored-By: Ada <ada@example.com>\n   ~~~~", "   ~~~~text\nRecorded risk co-author (quoted): Ada <ada@example.com>\n   ~~~~"},
		{"escaped closing backtick", "Recorded `literal \\` Co-Authored-By: Codex <bot@example.invalid>", "Recorded `literal \\` " + prAIAttributionNotice},
		{"unequal delimiter lengths", "Recorded ``literal ` Co-Authored-By: Codex <bot@example.invalid>", "Recorded ``literal ` " + prAIAttributionNotice},
		{"field prose", "The Co-Authored-By: field identifies contributors.", "The Co-Authored-By: field identifies contributors."},
		{"multiple addresses", "Recorded Co-Authored-By: Ada <ada@example.com> and Bob <bob@example.com>", "Recorded Co-Authored-By: Ada <ada@example.com> and Bob <bob@example.com>"},
		{"missing name", "Recorded Co-Authored-By: <ada@example.com>", "Recorded Co-Authored-By: <ada@example.com>"},
		{"incomplete address", "Recorded Co-Authored-By: Ada <ada>", "Recorded Co-Authored-By: Ada <ada>"},
		{"non-name prose", "Recorded Co-Authored-By: Ada; contact <ada@example.com>", "Recorded Co-Authored-By: Ada; contact <ada@example.com>"},
		{"field then claim", `The field "Co-Authored-By:" records Co-Authored-By: Ada <ada@example.com>.`, `The field "Co-Authored-By:" records co-author (quoted): Ada <ada@example.com>.`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := renderInlineCoAuthorKeys(tt.text, true); got != tt.want {
				t.Fatalf("inline claim rendering = %q, want %q", got, tt.want)
			}
		})
	}
}

// A co-author the body QUOTES is rendered as a labelled field.
//
// A consumer that scans the pull request body for attribution - the merge
// bar, a reviewer - reads a raw "Co-Authored-By:" line as a claim of AI
// attribution, and it reads it that way even inside a code fence, because it
// scans the raw text. So a co-author the body quotes - a test fixture, an
// example, a captured artifact - is rendered in a form no consumer reads as a
// claim.
//
// The quote stays a quote: the name and the address are the evidence, and they
// stay.
func TestPublishedBodyRendersAQuotedCoAuthorAsALabelledField(t *testing.T) {
	testing := "## Testing\n\n```text\nCo-Authored-By: Claude Smith <code@example.com>\n```\n"
	got := publishedBody("", "", testing, scm.ProviderGitHub)
	if strings.Contains(got, "Co-Authored-By:") {
		t.Fatalf("the published body carries a raw co-author trailer: %q", got)
	}
	if !strings.Contains(got, "co-author (quoted):") {
		t.Fatalf("the co-author was dropped instead of rendered as a labelled field: %q", got)
	}
	if !strings.Contains(got, "Claude Smith &lt;code@example.com&gt;") &&
		!strings.Contains(got, "Claude Smith <code@example.com>") {
		t.Fatalf("the co-author's name and address were lost: %q", got)
	}
}

// The body's OWN attribution is rendered too.
//
// The published body carries no raw co-author trailer line, wherever the
// co-author came from: the body quotes a fixture, or it carries its own
// attribution. A consumer that scans the body for attribution reads a raw
// "Co-Authored-By:" line as a claim, and it reads it that way wherever the
// line is. The name and the address survive the rendering; only the key is
// rendered.
func TestPublishedBodyRendersItsOwnAttributionAsALabelledField(t *testing.T) {
	body := "## What Changed\n\nPreserve the selected command environment.\nCo-Authored-By: Claude Smith <code@example.com>\n"
	risk := "Low: Recorded risk\n"
	got := publishedBody(body, risk, "", scm.ProviderGitHub)
	if strings.Contains(got, "Co-Authored-By:") {
		t.Fatalf("the published body carries a raw co-author trailer: %q", got)
	}
	if !strings.Contains(got, "co-author (quoted): Claude Smith <code@example.com>") {
		t.Fatalf("the body's own attribution lost its name or address: %q", got)
	}
}

// Every publication path renders the co-author key.
//
// The rule is one rule, so it has to hold on the paths that build a body
// differently: the full body, the minimal appendix, the collapsed appendix,
// and the appendix's own evidence. A body that reaches publication with a raw
// trailer line is a body a consumer reads as a claim.
func TestPublishedBodyRendersACoAuthorOnEveryAssemblyPath(t *testing.T) {
	for _, mode := range []string{config.PRAppendixFull, config.PRAppendixMinimal, config.PRAppendixCollapsed} {
		t.Run(mode, func(t *testing.T) {
			ctx := &pipeline.StepContext{Config: &config.Config{}}
			ctx.Config.PR.Appendix = mode
			narrative := "## What Changed\n\nCo-Authored-By: Claude Smith <code@example.com>\n"
			risk := "Low: Recorded risk Co-Authored-By: Claude <risk@example.com>\n"
			testingMD := "## Testing\n\nCo-Authored-By: Claude <artifact@example.com>\n"
			pipelineMD := "## Pipeline\n\nCo-Authored-By: Claude <pipeline@example.com>\n"
			bodyIdentities := []string{"Claude Smith <code@example.com>", "Claude <risk@example.com>"}
			evidenceIdentities := []string{"Claude <risk@example.com>"}
			if mode != config.PRAppendixMinimal {
				bodyIdentities = append(bodyIdentities, "Claude <artifact@example.com>", "Claude <pipeline@example.com>")
				evidenceIdentities = append(evidenceIdentities, "Claude <artifact@example.com>", "Claude <pipeline@example.com>")
			}
			for _, path := range []struct {
				name       string
				body       string
				identities []string
			}{
				{"assemble", assemblePRBody(ctx, narrative, risk, testingMD, pipelineMD, 0, scm.ProviderGitHub), bodyIdentities},
				{"build", buildPRBodyWithin(narrative, risk, testingMD, pipelineMD, ctx, scm.ProviderGitHub, maxPullRequestBodyBytes), bodyIdentities},
				{"evidence", appendixEvidence(mode, prBodyHTML, risk, testingMD, pipelineMD, scm.ProviderGitHub), evidenceIdentities},
			} {
				t.Run(path.name, func(t *testing.T) {
					if strings.TrimSpace(path.body) == "" {
						t.Fatal("the published body unexpectedly omitted all retained sections")
					}
					if strings.Contains(path.body, "Co-Authored-By:") {
						t.Fatalf("the published body carries a raw co-author trailer: %q", path.body)
					}
					for _, identity := range path.identities {
						if !strings.Contains(path.body, "co-author (quoted): "+identity) {
							t.Errorf("the published body lost the labelled name or address %q: %q", identity, path.body)
						}
					}
				})
			}
		})
	}
}

// The rendering happens BEFORE the body is budgeted.
//
// Each replacement grows the text. A body that fits the budget before the
// rendering must be truncated to it afterwards, not refused by the later
// budget validation.
func TestPublishedBodyRendersACoAuthorBeforeTheBudget(t *testing.T) {
	ctx := &pipeline.StepContext{Config: &config.Config{}}
	ctx.Config.PR.Appendix = config.PRAppendixFull
	const limit = 2000
	narrative := "## What Changed\n\n" + strings.Repeat("change ", 200) + "\nCo-Authored-By: Claude Smith <code@example.com>\n"
	for _, body := range []string{
		assemblePRBody(ctx, narrative, "low risk", "evidence", "## Pipeline\n\n- ok", limit, scm.ProviderGitHub),
		buildPRBodyWithin(narrative, "low risk", "evidence", "## Pipeline\n\n- ok", ctx, scm.ProviderGitHub, limit),
	} {
		if scm.PRBodyLen(body) > limit {
			t.Fatalf("the published body is over the budget: %d > %d\n%s", scm.PRBodyLen(body), limit, body)
		}
	}
}

// An AI attribution is still broken, not rendered.
//
// The classifier is unchanged: a line that IS an AI attribution is replaced by
// the neutralization notice, so the body claims nothing at all. The labelled
// field is for the lines the classifier deliberately keeps - a person's
// attribution, and an address whose form protects it from being the AI's.
func TestPublishedBodyStillBreaksAnAIAttribution(t *testing.T) {
	got := neutralizeAttestationMarkers(scm.ProviderGitHub, "Co-Authored-By: Codex <bot@example.invalid>")
	if strings.Contains(got, "Codex") {
		t.Fatalf("the published body still carries the AI's name: %q", got)
	}
	if !strings.Contains(got, "no-mistakes: AI attribution line neutralized") {
		t.Fatalf("the published body does not say the line was neutralized: %q", got)
	}
}

// An ordinary emoji is ordinary.
//
// The published text renders a quoted co-author as a labelled field; it does
// not go looking for characters and deleting them from prose. An emoji in a
// quoted line is part of the quote.
func TestPublishedBodyKeepsAnOrdinaryEmoji(t *testing.T) {
	testing := "## Testing\n\n```text\n\U0001F916 Robot integration passed\n```\n"
	got := publishedBody("", "", testing, scm.ProviderGitHub)
	if !strings.Contains(got, "\U0001F916 Robot integration passed") {
		t.Fatalf("the published body dropped an ordinary emoji: %q", got)
	}
}

// publishedBody assembles the body the way the pipeline publishes it, so the
// test measures the text a consumer of the pull request reads.
func publishedBody(body, risk, testing string, provider scm.Provider) string {
	return appendGeneratedSectionsToCleanBodyWithinLimit(body, risk, testing, "", 64*1024, provider)
}

func TestPRInlineClaimsAtSharedDisplayBoundary(t *testing.T) {
	for _, tt := range []struct{ raw, body, notice string }{
		{"Recorded Co-Authored-By: Codex <bot@example.invalid>.", "Recorded .", "Recorded " + prAIAttributionNotice + "."},
		{"Recorded Co-Authored-By: acp:cursor <ai@example.com>.", "Recorded .", "Recorded " + prAIAttributionNotice + "."},
		{"Recorded Co-Authored-By: Ada:Dev <ada@example.com>.", "Recorded co-author (quoted): Ada:Dev <ada@example.com>.", "Recorded co-author (quoted): Ada:Dev <ada@example.com>."},
		{`The field "Co-Authored-By:" identifies acp:cursor <ai@example.com>`, `The field "Co-Authored-By:" identifies acp:cursor <ai@example.com>`, `The field "Co-Authored-By:" identifies acp:cursor <ai@example.com>`},
		{"Recorded `Co-Authored-By: acp:cursor <ai@example.com>`", "Recorded `Co-Authored-By: acp:cursor <ai@example.com>`", "Recorded `Co-Authored-By: acp:cursor <ai@example.com>`"},
		{"Recorded Co-Authored-By: Ada; contact <ada@example.com>", "Recorded Co-Authored-By: Ada; contact <ada@example.com>", "Recorded Co-Authored-By: Ada; contact <ada@example.com>"},
	} {
		for _, notice := range []bool{false, true} {
			want := tt.body
			if notice {
				want = tt.notice
			}
			if got := prDisplayTextMode(tt.raw, notice); got != want {
				t.Errorf("notice=%t: got %q, want %q", notice, got, want)
			}
			encoded, err := marshalJSONString(tt.raw)
			if err != nil {
				t.Fatal(err)
			}
			expected, err := marshalJSONString(want)
			if err != nil {
				t.Fatal(err)
			}
			prefix, suffix := `{ "summary": `, `, "escape": "A\u0042\/C", "number": 1.200e+3 }`
			if got := prDisplayTextMode(prefix+string(encoded)+suffix, notice); got != prefix+string(expected)+suffix {
				t.Errorf("recorded notice=%t: got %q", notice, got)
			}
		}
	}
}

func TestPRInlineClaimsOwnedUpdateAndRestamp(t *testing.T) {
	const raw = "Recorded risk Co-Authored-By: Codex <bot@example.invalid>\nRecorded tool Co-Authored-By: acp:cursor <ai@example.com>\nRecorded helper Co-Authored-By: Ada <ada@example.com>.\n"
	const want = "Recorded risk \nRecorded tool \nRecorded helper co-author (quoted): Ada <ada@example.com>.\n"
	const payload = `{ "summary": "Recorded tool Co-Authored-By: acp:cursor \u003cai@example.com\u003e", "escape": "A\u0042\/C" }`
	const wantPayload = `{ "summary": "Recorded tool ", "escape": "A\u0042\/C" }`
	for _, provider := range []scm.Provider{scm.ProviderGitHub, scm.ProviderBitbucket} {
		t.Run(string(provider), func(t *testing.T) {
			appendix := compliantPipelineBody(t, testPipelineHeadSHA)
			author := raw + payload + "\n"
			parts := prOwnedBody{before: author, after: "\n" + author}
			created, err := composeOwnedPRContent(parts, "fix: publication", appendix+"\n"+author, 0)
			if err != nil {
				t.Fatal(err)
			}
			check := func(body string, owned bool) {
				t.Helper()
				if strings.Contains(body, "Co-Authored-By:") || strings.Contains(body, "Codex") || strings.Contains(body, "acp:cursor") || strings.Contains(body, "ai@example.com") || strings.Contains(body, "bot@example.invalid") {
					t.Fatalf("publication retained AI attribution: %q", body)
				}
				if !strings.Contains(body, want+wantPayload) {
					t.Fatalf("publication lost human identity or unrelated bytes: %q", body)
				}
				if owned {
					if _, err := parsePROwnedBody(body); err != nil {
						t.Fatalf("invalid ownership integrity: %v", err)
					}
				}
			}
			check(created.Body, true)
			old := author + wrapPRAppendix(appendix+"\n"+author) + "\n" + author
			host := &attestationTestHost{provider: provider, body: old}
			if err := updateOwnedPR(&pipeline.StepContext{Ctx: context.Background()}, host, &scm.PR{Number: "42"}, scm.PRContent{Body: old}, "", "", appendix+"\n"+author, 0, nil); err != nil {
				t.Fatal(err)
			}
			check(host.body, true)
			for _, prior := range []string{old, author + appendix + "\n" + author} {
				updated, rebound, err := rebindOwnedPRAttestation(prior, strings.Repeat("b", 40), nil, pipelineAttestationPolicy{}, provider)
				if err != nil || !rebound {
					t.Fatalf("restamp: rebound=%t err=%v", rebound, err)
				}
				check(updated, hasPRAppendixMarkers(prior))
			}
		})
	}
}
