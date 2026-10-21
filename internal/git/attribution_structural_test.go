package git

import (
	"os/exec"
	"strings"
	"testing"
)

func TestConfiguredAttributionGitStructure(t *testing.T) {
	for _, tc := range []struct {
		name, source, footer string
		remove               bool
	}{
		{"lowercase", "Originated-by: bespoke-bot", "originated-by: bespoke-bot", true},
		{"uppercase", "Originated-by: bespoke-bot", "ORIGINATED-BY: bespoke-bot", true},
		{"before-colon", "Originated-by: bespoke-bot", "Originated-by : bespoke-bot", true},
		{"after-colon", "Originated-by: bespoke-bot", "Originated-by:\tbespoke-bot", true},
		{"template", "Originated-by: {{.Agent}}/{{.Model}}", "ORIGINATED-BY : bespoke/model-1", true},
		{"folded", "Originated-by: bespoke-bot model", "originated-by:\tbespoke-bot\n model", true},
		{"human", "Originated-by: bespoke-bot", "originated-by: Human Person", false},
		{"value-case", "Originated-by: bespoke-bot", "Originated-by: BESPOKE-BOT", false},
		{"unconfigured", "", "originated-by: bespoke-bot", false},
		{"bounded-template", "Originated-by: {{.Agent}}", "originated-by: bespoke bot", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			message := "subject\n\n" + tc.footer + "\n"
			cmd := exec.Command("git", "interpret-trailers", "--if-exists=replace", "--trailer", "Originated-by: oracle")
			cmd.Dir = t.TempDir()
			cmd.Stdin = strings.NewReader(message)
			out, err := cmd.CombinedOutput()
			if err != nil || string(out) != "subject\n\nOriginated-by: oracle\n" {
				t.Fatalf("Git key equivalence = %q, %v", out, err)
			}
			want := message
			if tc.remove {
				want = "subject"
			}
			if got := StripAIAttributionTrailers(message, tc.source); got != want {
				t.Fatalf("filtered = %q, want %q", got, want)
			}
		})
	}
}
