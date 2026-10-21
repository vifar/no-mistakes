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
		{"Assisted-by: antigravity:model", true},
		{"Assisted-by: rovodev:model", true},
		{"Assisted-by: acp:cursor:model", true},
		{"Assisted-by: no-mistakes:acp:cursor:model", true},
		{"Co-Authored-By: no-mistakes antigravity <ai@example.com>", true},
		{"Co-Authored-By: no-mistakes rovodev <ai@example.com>", true},
		{"Co-Authored-By: no-mistakes acp:cursor:model <ai@example.com>", true},
		{"Co-Authored-By: acp:cursor <ai@example.com>", true},
		{"Generated with antigravity", true},
		{"Generated with rovodev", true},
		{"Generated with acp:cursor", true},
		{"Co-Authored-By: Antigravity Smith <human@example.com>", false},
		{"Assisted-by: RovoDev Smith <human@example.com>", false},
		{"Generated with acp:", false},
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

func TestStripAIAttributionTrailersConfiguredIdentifiers(t *testing.T) {
	t.Parallel()
	input := "repair widget\n\nDisclosure: fixtures were generated with the repository recorder.\n\nOriginated-by: bespoke-bot\nCustom-agent: acp:cursor:model\nCo-Authored-By: Some Person <person@example.com>\nSigned-off-by: Some Person <person@example.com>"
	want := "repair widget\n\nDisclosure: fixtures were generated with the repository recorder.\n\nCo-Authored-By: Some Person <person@example.com>\nSigned-off-by: Some Person <person@example.com>"
	if got := StripAIAttributionTrailers(input, "Originated-by: bespoke-bot", "Custom-agent: {{ .Agent }}:{{ .Model }}"); got != want {
		t.Fatalf("message = %q, want %q", got, want)
	}
}

func TestAttributionBoundaryStandaloneModel(t *testing.T) {
	input := "repair widget\n\nCo-Authored-By: gpt-5.5 <model@example.com>\nCo-Authored-By: Claude Smith <person@example.com>\nCo-Authored-By: Claude <claude@example.com>"
	want := "repair widget\n\nCo-Authored-By: Claude Smith <person@example.com>\nCo-Authored-By: Claude <claude@example.com>"
	if got := StripAIAttributionTrailers(input); got != want {
		t.Fatalf("unconfigured model identity = %q, want %q", got, want)
	}
}
