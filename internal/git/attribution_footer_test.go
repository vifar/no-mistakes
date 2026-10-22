package git

import (
	"os/exec"
	"strings"
	"testing"
)

// Git's executable consumer is the independent oracle for footer context.
// Exact expected messages also protect prose, comments and human credit from
// a parser that merely deletes every attribution-looking line.
func TestStripAIAttributionGitFooterContext(t *testing.T) {
	const signoff = "Signed-off-by: Hook Person <hook@example.com>"
	for _, tc := range []struct {
		name, body, parsed, want string
	}{
		{"reported", "Hook body edit.\n" + signoff + "\nCodex-Session: hook", signoff + "\nCodex-Session: hook\n", "Hook body edit.\n" + signoff},
		{"cherry-pick-footer", "Hook body edit.\n(cherry picked from commit abcdef)\nCodex-Session: hook", "Codex-Session: hook\n", "Hook body edit.\n(cherry picked from commit abcdef)"},
		{"identities-and-configured", "Hook body edit.\n" + signoff + "\nCo-Authored-By: gpt-5.5\n <model@example.com>\nOriginated-by: bespoke-bot\nCo-Authored-By: Claude Smith <human@example.com>\nDisclosure: fixtures were generated with the repository recorder.", signoff + "\nCo-Authored-By: gpt-5.5 <model@example.com>\nOriginated-by: bespoke-bot\nCo-Authored-By: Claude Smith <human@example.com>\nDisclosure: fixtures were generated with the repository recorder.\n", "Hook body edit.\n" + signoff + "\nCo-Authored-By: Claude Smith <human@example.com>\nDisclosure: fixtures were generated with the repository recorder."},
		{"comments", "Hook body edit.\n" + signoff + "\n# keep comment\nCodex-Session: hook\n# keep final comment", signoff + "\nCodex-Session: hook\n", "Hook body edit.\n" + signoff + "\n# keep comment\n# keep final comment"},
		{"separate-final-comment", "Hook body edit.\n" + signoff + "\nCodex-Session: hook\n\n# keep final comment", signoff + "\nCodex-Session: hook\n", "Hook body edit.\n" + signoff + "\n\n# keep final comment"},
		{"comment-only-prefix", "# keep comment\nCodex-Session: hook", "Codex-Session: hook\n", "# keep comment"},
		{"comment-only-suffix", "Codex-Session: hook\n# keep comment", "Codex-Session: hook\n", "# keep comment"},
		{"interleaved-prose", "Hook body edit.\n" + signoff + "\nMore body prose.\nCodex-Session: hook", signoff + "\nCodex-Session: hook\n", "Hook body edit.\n" + signoff + "\nMore body prose."},
		{"quarter-trailers", strings.Repeat("Body prose.\n", 6) + signoff + "\nCodex-Session: hook", signoff + "\nCodex-Session: hook\n", strings.Repeat("Body prose.\n", 6) + signoff},
		{"too-much-prose", strings.Repeat("Body prose.\n", 7) + signoff + "\nCodex-Session: hook", "", ""},
		{"unknown-keys-in-prose", "Hook body edit.\nCodex-Session: hook", "", ""},
		{"quoted-example", "> Signed-off-by: Hook Person <hook@example.com>\n> Codex-Session: hook", "", ""},
		// Git recognizes the unquoted keys inside a fence; our stronger body
		// preservation contract must still keep the complete example intact.
		{"fenced-example", "```text\n" + signoff + "\nCodex-Session: hook\n```", signoff + "\nCodex-Session: hook\n", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			message := "subject\n\n" + tc.body
			cmd := exec.Command("git", "interpret-trailers", "--parse")
			cmd.Dir = t.TempDir()
			cmd.Stdin = strings.NewReader(message)
			out, err := cmd.CombinedOutput()
			if err != nil || string(out) != tc.parsed {
				t.Fatalf("Git footer oracle = %q, %v; want %q", out, err, tc.parsed)
			}
			want := message
			if tc.want != "" {
				want = "subject\n\n" + tc.want
			}
			if got := StripAIAttributionTrailers(message, "Originated-by: bespoke-bot"); got != want {
				t.Fatalf("message = %q, want %q", got, want)
			}
		})
	}
}
