package git

import (
	"os/exec"
	"strings"
	"testing"
)

func TestAttributionFenceFooterContext(t *testing.T) {
	const human = "Signed-off-by: Hook Person <hook@example.com>"
	const credit = "Originated-by : bespoke-bot"
	for _, tc := range []struct{ name, open, close, gap, inside string }{
		{"backticks", "```text", "```", "", "example"},
		{"tildes", "~~~text", "~~~", "", "example"},
		{"longer-close", "````text", "`````", "", "```\nCodex-Session: enclosed"},
		{"indented", "  ~~~~text", "  ~~~~", "", "Co-Authored-By: gpt-5.5 <model@example.com>"},
		{"blank", "```", "```", "\n", "Codex-Session: enclosed"},
		{"inside-blank", "~~~", "~~~", "", "body\n\nCodex-Session: enclosed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			example := tc.open + "\n" + tc.inside + "\n" + tc.close + "\n" + tc.gap
			footer := human + "\nCodex-Session: outside\n" + credit
			message := "subject\n\n" + example + footer
			cmd := exec.Command("git", "interpret-trailers", "--parse")
			cmd.Dir = t.TempDir()
			cmd.Stdin = strings.NewReader(message)
			out, err := cmd.CombinedOutput()
			if err != nil || !strings.Contains(string(out), "Codex-Session: outside\n") || !strings.Contains(string(out), "Originated-by: bespoke-bot\n") {
				t.Fatalf("Git footer oracle = %q, %v", out, err)
			}
			want := "subject\n\n" + example + human
			if got := StripAIAttributionTrailers(message, "Originated-by: bespoke-bot"); got != want {
				t.Fatalf("filtered = %q, want %q", got, want)
			}
		})
	}
	for _, subject := range []string{"``` literal title", "~~~ literal title"} {
		message := subject + "\n\nSigned-off-by: Hook Person <hook@example.com>\nCodex-Session: outside"
		want := subject + "\n\nSigned-off-by: Hook Person <hook@example.com>"
		if got := StripAIAttributionTrailers(message); got != want {
			t.Fatalf("subject changed footer parsing: %q", got)
		}
	}

	for _, example := range []string{
		"```text\nSigned-off-by: Hook Person <hook@example.com>\nCodex-Session: enclosed\n```",
		"~~~\n\nCodex-Session: enclosed\n~~~",
		"```text\nCodex-Session: enclosed\n~~j",
	} {
		message := "subject\n\n" + example
		if got := StripAIAttributionTrailers(message); got != message {
			t.Fatalf("changed enclosed example: %q", got)
		}
	}
}
