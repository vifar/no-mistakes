package steps

import (
	"strings"
	"testing"
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
		{"generated with", "Generated with [Claude Code]"},
		{"generated with plain", "Generated with Claude Code"},
		{"robot emoji", "\U0001F916 Generated with Codex"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := neutralizeAIAttribution(tc.line)
			if strings.Contains(got, "Codex") || strings.Contains(got, "Claude") ||
				strings.Contains(got, "Anthropic") || strings.Contains(got, "OpenAI") ||
				strings.Contains(got, "ChatGPT") || strings.Contains(got, "Grok") ||
				strings.Contains(got, "Copilot") {
				t.Fatalf("the line still names an AI: %q", got)
			}
			if !strings.Contains(got, "neutralized") {
				t.Fatalf("the line was not marked as neutralized: %q", got)
			}
		})
	}
}

// A trailer that names a PERSON is legitimate attribution and stays.
func TestNeutralizeAIAttributionKeepsAHumanTrailer(t *testing.T) {
	line := "Co-Authored-By: A Person <person@example.com>"
	if got := neutralizeAIAttribution(line); got != line {
		t.Fatalf("a human trailer was changed: %q", got)
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
