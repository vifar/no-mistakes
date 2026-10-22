package steps

import (
	"html"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

// The pull request body is generated, but the evidence it embeds is not: a
// test that exercises the attribution feature quotes the trailer it created.
// Every consumer that scans the body for attribution - the merge bar, a
// reviewer - then reads that quoted trailer as an attribution of THIS work,
// and refuses a pull request whose author wrote none.
//
// The line is broken, not dropped: the evidence still shows what the test did.
func TestNeutralizeAIAttributionBreaksTheLinesAReaderTakesForAttribution(t *testing.T) {
	for _, tc := range []struct {
		name string
		line string
	}{
		{"co-authored trailer", "Co-Authored-By: Codex <bot@example.invalid>"},
		{"co-authored trailer with a name", "Co-Authored-By: Claude <noreply@anthropic.com>"},
		{"generated with", "Generated with [Claude Code](https://claude.ai/code)"},
		{"generated with plain", "Generated with Claude Code"},
		{"generated with codex", "Generated with Codex"},
		{"model trailer", "Co-Authored-By: gpt-5.5 <model@example.com>"},
		{"assisted trailer", "Assisted-by: OpenCode"},
		{"claude session", "Claude-Session: session-id"},
		{"codex session", "Codex-Session: session-id"},
		{"robot emoji", "\U0001F916 Generated with Codex"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, line := range []string{tc.line, html.EscapeString(tc.line)} {
				body := "## Testing\n\n```text\n" + line + "\n```\n"
				got := neutralizeAttestationMarkers(scm.ProviderGitHub, body)
				want := "## Testing\n\n```text\n<no-mistakes: AI attribution line neutralized>\n```\n"
				if got != want {
					t.Fatalf("published text = %q, want %q", got, want)
				}
			}
		})
	}
}

// A trailer that names a PERSON is legitimate attribution and stays.
func TestNeutralizeAIAttributionKeepsAHumanTrailer(t *testing.T) {
	for _, line := range []string{
		"Co-Authored-By: A Person <person@example.com>",
		"Co-Authored-By: Claude Smith <human@example.com>",
		"Co-Authored-By: Claude <human@example.com>",
		"Assisted-by: Claude Smith <human@example.com>",
		"We tested fixtures generated with Codex.",
	} {
		for _, text := range []string{line, html.EscapeString(line)} {
			if got := neutralizeAttestationMarkers(scm.ProviderGitHub, text); got != text {
				t.Fatalf("human attribution or prose was changed: %q", got)
			}
		}
	}
}

// The neutralization applies to the whole body, so the evidence section is
// covered as well as the prose.
func TestNeutralizeAIAttributionAppliesInsideEmbeddedEvidence(t *testing.T) {
	body := "## Testing\n\n```text\nCo-Authored-By: Codex <bot@example.invalid>\n```\n"
	got := neutralizeAIAttribution(body)
	if strings.Contains(got, "Codex") {
		t.Fatalf("the embedded evidence still names an AI: %q", got)
	}
	if !strings.Contains(got, "## Testing") {
		t.Fatalf("the body lost its structure: %q", got)
	}
}
