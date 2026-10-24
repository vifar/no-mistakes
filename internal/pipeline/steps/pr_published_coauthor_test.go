package steps

import (
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
)

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
		{"field prose", "The Co-Authored-By: field identifies contributors.", "The Co-Authored-By: field identifies contributors."},
		{"multiple addresses", "Recorded Co-Authored-By: Ada <ada@example.com> and Bob <bob@example.com>", "Recorded Co-Authored-By: Ada <ada@example.com> and Bob <bob@example.com>"},
		{"missing name", "Recorded Co-Authored-By: <ada@example.com>", "Recorded Co-Authored-By: <ada@example.com>"},
		{"incomplete address", "Recorded Co-Authored-By: Ada <ada>", "Recorded Co-Authored-By: Ada <ada>"},
		{"non-name prose", "Recorded Co-Authored-By: Ada; contact <ada@example.com>", "Recorded Co-Authored-By: Ada; contact <ada@example.com>"},
		{"field then claim", `The field "Co-Authored-By:" records Co-Authored-By: Ada <ada@example.com>.`, `The field "Co-Authored-By:" records co-author (quoted): Ada <ada@example.com>.`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := renderInlineCoAuthorKeys(tt.text); got != tt.want {
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
