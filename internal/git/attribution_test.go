package git

import "testing"

// Complete identity and line matching is shared with summary writers (#98).
// The independent examples below reject both substring false positives and
// missed configured/session attribution from the issue #80 acceptance contract.
func TestIsAIAttributionTrailer(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		line string
		ai   bool
	}{
		{"Co-Authored-By: Claude <noreply@anthropic.com>", true},
		{"Co-authored-by: Codex <ai@example.com>", true},
		{"Co-Authored-By: Claude Smith <claude@example.com>", false},
		{"Co-Authored-By: Claude <claude@example.com>", false},
		{"Co-Authored-By: Some Person <openai@example.com>", false},
		{"Assisted-by: pi:local", true},
		{"Assisted-by: no-mistakes:codex:gpt-6.1-sol", true},
		{"Assisted-by: Some Person <claude@example.com>", false},
		{"Claude-Session: abc", true},
		{"Codex-Session: abc", true},
		{"Human-Session: abc", false},
		{"Generated with Claude Code", true},
		{"🤖 Generated with [Claude Code](https://claude.ai/code)", true},
		{"Generated with the repository recorder", false},
		{"Disclosure: fixtures were generated with the repository recorder.", false},
		{"Disclosure: Generated with Claude Code is the forbidden marker.", false},
		{"Discuss Claude-Session: abc", false},
		{"This fixes Assisted-by: pi:local handling", false},
	} {
		t.Run(tc.line, func(t *testing.T) {
			if got := IsAIAttributionTrailer(tc.line); got != tc.ai {
				t.Fatalf("classification = %v, want %v", got, tc.ai)
			}
		})
	}
}

func TestStripAIAttributionTrailersPreservesUnchangedBytes(t *testing.T) {
	t.Parallel()
	message := "repair widget\n\nDisclosure: fixtures were generated with the repository recorder.\n\nCo-Authored-By: Claude Smith <claude@example.com>\n\n"
	if got := StripAIAttributionTrailers(message); got != message {
		t.Fatalf("message = %q, want exact %q", got, message)
	}
}

func TestStripAIAttributionTrailersSeparateFooterParagraphs(t *testing.T) {
	t.Parallel()
	input := "repair widget\n\nKeep the reason.\n\nCo-Authored-By: Claude <noreply@anthropic.com>\n\nGenerated with Claude Code\n\nSigned-off-by: Some Person <person@example.com>"
	want := "repair widget\n\nKeep the reason.\n\nSigned-off-by: Some Person <person@example.com>"
	if got := StripAIAttributionTrailers(input); got != want {
		t.Fatalf("message = %q, want %q", got, want)
	}
}
