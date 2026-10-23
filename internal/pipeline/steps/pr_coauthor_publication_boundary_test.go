package steps

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

// The publication boundary must recognise EVERY representation of an
// attribution it publishes, not only the raw trailer form: a recorded JSON
// payload without escaped newlines, and a value an earlier publication already
// rendered with the labelled field, both reach the same published body.
func TestPRPublishedBodyFiltersRecordedPayloadWithoutEscapedNewlines(t *testing.T) {
	t.Parallel()
	recordedAI := `{"description":"Co-Authored-By: gpt-5.5 <model@example.com>","file":"internal/x.go"}`
	got := prDisplayBody(recordedAI)
	if strings.Contains(got, "model@example.com") || strings.Contains(strings.ToLower(got), "gpt-5.5") {
		t.Errorf("recorded AI attribution survived without escaped newlines: %q", got)
	}
	if !strings.Contains(got, "internal/x.go") {
		t.Errorf("recorded payload lost its own fields: %q", got)
	}

	recordedHuman := `{"description":"Co-Authored-By: Claude Smith <claude@example.com>"}`
	got = prDisplayBody(recordedHuman)
	if strings.Contains(got, "Co-Authored-By:") {
		t.Errorf("recorded human co-author published as a raw trailer: %q", got)
	}
	if !strings.Contains(got, "co-author (quoted): Claude Smith") || !strings.Contains(got, "claude@example.com") {
		t.Errorf("recorded human co-author lost from the published payload: %q", got)
	}
}

func TestPRPublishedBodyRemovesAlreadyLabelledAIAttribution(t *testing.T) {
	t.Parallel()
	for _, ai := range []string{
		"co-author (quoted): gpt-5.5 <model@example.com>",
		"co-author (quoted): Claude <noreply@anthropic.com>",
		"**co-author (quoted): Codex <codex@example.com>**",
		`> "co-author (quoted): Gemini <gemini@example.com>"`,
	} {
		got := prDisplayBody("Evidence:\n" + ai + "\n\n" + humanCoauthorDisclosure)
		lower := strings.ToLower(got)
		for _, forbidden := range []string{"model@example.com", "noreply@anthropic.com", "codex@example.com", "gemini@example.com", "gpt-5.5", "codex", "gemini", "co-author"} {
			if strings.Contains(lower, forbidden) {
				t.Errorf("already-labelled AI attribution survived %q: %q", ai, got)
			}
		}
		if !strings.Contains(got, humanCoauthorDisclosure) {
			t.Errorf("required disclosure lost beside %q: %q", ai, got)
		}
	}
}

func TestPRPublishedBodyKeepsAlreadyLabelledHumanCoauthor(t *testing.T) {
	t.Parallel()
	for _, human := range []string{
		"co-author (quoted): Claude Smith <claude@example.com>",
		"co-author (quoted): Claude\n <claude@example.com>",
		"> co-author (quoted): Claude Smith <claude@example.com>",
	} {
		got := prDisplayBody(human)
		if strings.Contains(got, "Co-Authored-By:") {
			t.Errorf("published human co-author still resembles a trailer: %q", got)
		}
		if !strings.Contains(got, "co-author (quoted): Claude") || !strings.Contains(got, "claude@example.com") {
			t.Errorf("labelled human co-author lost: %q, want %q", got, human)
		}
	}
}

func TestPRPublishedBareCoauthorEntries(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ input, want, notice string }{
		{"co-author (quoted): Claude", "", prAIAttributionNotice},
		{"<code>Co-Authored-By: Claude</code>", "", prAIAttributionNotice},
		{"> Co-Authored-By: Claude", "", prAIAttributionNotice},
		{"- Co-Authored-By: Claude", "", prAIAttributionNotice},
		{"co-author (quoted): **Codex**", "", prAIAttributionNotice},
		{"co-author (quoted): Claude\n <noreply@anthropic.com>", "", prAIAttributionNotice},
		{"co-author (quoted): Claude Smith <claude@example.com>", "co-author (quoted): Claude Smith <claude@example.com>", "co-author (quoted): Claude Smith <claude@example.com>"},
		{"co-author (quoted): Claude\n <claude@example.com>", "co-author (quoted): Claude\n <claude@example.com>", "co-author (quoted): Claude\n <claude@example.com>"},
		{"<code>Co-Authored-By: Jane Doe</code>", "<code>co-author (quoted): Jane Doe</code>", "<code>co-author (quoted): Jane Doe</code>"},
		{`{"description":"co-author (quoted): Claude"}`, `{"description":""}`, `{"description":"<no-mistakes: AI attribution line neutralized>"}`},
		{` "co-author (quoted): Claude",`, ` "",`, ` "<no-mistakes: AI attribution line neutralized>",`},
		{` "co-author (quoted): Jane Doe\nco-author (quoted): Claude",`, ` "co-author (quoted): Jane Doe\n",`, ` "co-author (quoted): Jane Doe\n<no-mistakes: AI attribution line neutralized>",`},
		{`{"description":"co-author (quoted): Claude\n <noreply@anthropic.com>"}`, `{"description":""}`, `{"description":"<no-mistakes: AI attribution line neutralized>"}`},
		{"Ordinary prose mentioning co-author (quoted): Claude stays verbatim.", "Ordinary prose mentioning co-author (quoted): Claude stays verbatim.", "Ordinary prose mentioning co-author (quoted): Claude stays verbatim."},
	} {
		t.Run(tc.input, func(t *testing.T) {
			if got := prDisplayBody(tc.input); got != tc.want {
				t.Errorf("published body = %q, want %q", got, tc.want)
			}
			if got := prDisplayNoticeText(tc.input); got != tc.notice {
				t.Errorf("published evidence = %q, want %q", got, tc.notice)
			}
		})
	}
}

func TestPRNeutralizationKeepsFoldedCoauthorContext(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ input, want string }{
		{"co-author (quoted): Claude", prAIAttributionNotice},
		{"co-author (quoted): Claude\n <noreply@anthropic.com>", prAIAttributionNotice},
		{"co-author (quoted): Claude\n <claude@example.com>", "co-author (quoted): Claude\n <claude@example.com>"},
		{"> Co-Authored-By: Claude\n> <claude@example.com>\n> Focused checks passed", "> Co-Authored-By: Claude\n> <claude@example.com>\n> Focused checks passed"},
		{`"Co-Authored-By:" is a field name.`, `"Co-Authored-By:" is a field name.`},
	} {
		t.Run(tc.input, func(t *testing.T) {
			if got := neutralizeAIAttribution(tc.input); got != tc.want {
				t.Errorf("neutralized evidence = %q, want %q", got, tc.want)
			}
		})
	}
}

// The filter must see the original bytes: matching a configured trailer after
// path redaction rewrote it silently preserves the forbidden entry.
func TestOwnedCompositionFiltersConfiguredTrailerBeforeRedaction(t *testing.T) {
	t.Parallel()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory")
	}
	configured := "Build-Agent: " + filepath.Join(home, "private-identifier")
	appendix := compliantPipelineBody(t, testPipelineHeadSHA)
	content, err := composeOwnedPRContent(prOwnedBody{before: "author text\n" + configured}, "fix: x", appendix, 0, configured)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(content.Body, "private-identifier") {
		t.Errorf("configured trailer survived filter-after-redaction composition: %q", content.Body)
	}
	if !strings.Contains(content.Body, "author text") {
		t.Error("owned composition lost author text")
	}
}

func TestOwnedRestampFiltersConfiguredTrailerBeforeRedaction(t *testing.T) {
	t.Parallel()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory")
	}
	configured := "Build-Agent: " + filepath.Join(home, "private-identifier")
	appendix := compliantPipelineBody(t, testPipelineHeadSHA) + "\n" + configured
	body := "author text\n\n" + wrapPRAppendix(appendix) + "\nafter"
	policy := pipelineAttestationPolicy{ConfiguredTrailers: []string{configured}}
	updated, rebound, err := rebindOwnedPRAttestation(body, strings.Repeat("b", 40), nil, policy, scm.ProviderGitHub)
	if err != nil || !rebound {
		t.Fatalf("restamp failed: %t, %v", rebound, err)
	}
	if strings.Contains(updated, "private-identifier") {
		t.Errorf("configured trailer survived filter-after-redaction restamp: %q", updated)
	}
}
