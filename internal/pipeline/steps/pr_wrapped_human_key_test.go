package steps

import (
	"encoding/json"
	"html"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

func TestPRWrappedHumanKeyPresentation(t *testing.T) {
	for _, wrapper := range [][2]string{{"**", "**"}, {"`", "`"}, {"<code>", "</code>"}, {"__", "__"}, {"*", "*"}, {"~~", "~~"}, {"**`", "`.**"}} {
		for _, suffix := range []string{"", " <jane@example.com>.", "\n <jane@example.com>!"} {
			for _, colonInside := range []bool{false, true} {
				key, labelled := wrapper[0]+"Co-Authored-By"+wrapper[1]+":", wrapper[0]+"co-author (quoted)"+wrapper[1]+":"
				if colonInside {
					key, labelled = wrapper[0]+"Co-Authored-By:"+wrapper[1], wrapper[0]+"co-author (quoted):"+wrapper[1]
				}
				for _, escaped := range []bool{false, true} {
					input, want := key+" Jane"+suffix, labelled+" Jane"+suffix
					ai := key + " Codex" + strings.ReplaceAll(suffix, "jane@example.com", "ai@example.com")
					if escaped {
						input, want, ai = html.EscapeString(input), html.EscapeString(want), html.EscapeString(ai)
					}
					t.Run(input, func(t *testing.T) {
						for _, render := range []func(string) string{
							func(text string) string { return prDisplayBody(text) },
							func(text string) string { return prDisplayNoticeText(text) },
							renderCoAuthorAsLabelledField,
							func(text string) string { return neutralizeAttestationMarkers(scm.ProviderGitHub, text) },
						} {
							if got := render(input); got != want {
								t.Errorf("human field = %q, want %q", got, want)
							}
							if got := render(want); got != want {
								t.Errorf("published human field changed on reuse: %q", got)
							}
						}
						if got := prDisplayBody(ai); got != "" {
							t.Errorf("AI attribution survived: %q", got)
						}
						if got := neutralizeAttestationMarkers(scm.ProviderGitHub, ai); got != prAIAttributionNotice {
							t.Errorf("AI narrative = %q", got)
						}
						for _, tc := range []struct{ input, want string }{{input, want}, {ai, ""}} {
							encoded, err := json.Marshal(tc.input)
							if err != nil {
								t.Fatal(err)
							}
							for _, payload := range []string{`{"description":` + string(encoded) + `}`, " " + string(encoded) + ","} {
								got := prDisplayBody(payload)
								var value string
								if strings.HasPrefix(payload, "{") {
									var fields map[string]string
									if err := json.Unmarshal([]byte(got), &fields); err != nil {
										t.Fatal(err)
									}
									value = fields["description"]
								} else if err := json.Unmarshal([]byte(strings.TrimSuffix(strings.TrimSpace(got), ",")), &value); err != nil {
									t.Fatal(err)
								}
								if value != tc.want {
									t.Errorf("recorded field = %q, want %q", value, tc.want)
								}
							}
						}
						if got := stripReviewAIAttribution(input, nil); got != input {
							t.Errorf("commit human field changed: %q", got)
						}
					})
				}
			}
		}
	}
	for _, prose := range []string{
		`"Co-Authored-By:" is a field name.`,
		`> "Co-Authored-By:" is a field name.`,
		`- "Co-Authored-By:" is a field name.`,
		"`\"Co-Authored-By:\" is a field name.`",
		"Ordinary prose mentions **Co-Authored-By**: Jane <jane@example.com>.",
		"[**Co-Authored-By**: Jane <jane@example.com>](https://example.com).",
		"**Co-Authored-By**:",
		"co-author (quoted): Jane Co-Authored-By <jane@example.com>",
	} {
		if got := prDisplayBody(prose); got != prose {
			t.Errorf("prose or labelled name changed: %q", got)
		}
	}
}
