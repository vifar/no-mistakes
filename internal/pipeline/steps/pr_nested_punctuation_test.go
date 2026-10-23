package steps

import (
	"encoding/json"
	"html"
	"strings"
	"testing"
)

func TestPRNestedPunctuationPublication(t *testing.T) {
	for _, outer := range [][2]string{{"**", "**"}, {"__", "__"}, {"*", "*"}, {"_", "_"}, {"~~", "~~"}, {"`", "`"}, {"``", "``"}, {"<code>", "</code>"}} {
		for _, inner := range [][2]string{{"**", "**"}, {"_", "_"}, {"~~", "~~"}, {"`", "`"}, {"<code>", "</code>"}} {
			wrap := func(value string) string {
				return "!" + outer[0] + "?" + inner[0] + value + inner[1] + "." + outer[1] + ";"
			}
			for _, key := range []string{"Co-Authored-By:", "co-author (quoted):"} {
				for _, separator := range []string{" ", "\n "} {
					for _, shape := range []string{"whole", "fields", "key-and-name", "name-and-mailbox"} {
						ai, human := key+" Codex"+separator+"<ai@example.com>", key+" Jane"+separator+"<jane@example.com>"
						switch shape {
						case "whole":
							ai, human = wrap(ai), wrap(human)
						case "fields":
							ai = wrap(key) + " " + wrap("Codex") + separator + wrap("<ai@example.com>")
							human = wrap(key) + " " + wrap("Jane") + separator + wrap("<jane@example.com>")
						case "key-and-name":
							ai = wrap(key+" Codex") + separator + wrap("<ai@example.com>")
							human = wrap(key+" Jane") + separator + wrap("<jane@example.com>")
						case "name-and-mailbox":
							ai = wrap(key) + " " + wrap("Codex"+separator+"<ai@example.com>")
							human = wrap(key) + " " + wrap("Jane"+separator+"<jane@example.com>")
						}
						want := strings.Replace(human, key, "co-author (quoted):", 1)
						for _, escaped := range []bool{false, true} {
							input, retained, expected := ai, human, want
							if escaped {
								input, retained, expected = html.EscapeString(input), html.EscapeString(retained), html.EscapeString(expected)
							}
							t.Run(input, func(t *testing.T) {
								if got := prDisplayBody(input); got != "" {
									t.Errorf("AI attribution survived: %q", got)
								}
								if got := prDisplayNoticeText(input); got != prAIAttributionNotice {
									t.Errorf("evidence attribution = %q", got)
								}
								if got := neutralizeAIAttribution(input); got != prAIAttributionNotice {
									t.Errorf("neutralized attribution = %q", got)
								}
								if got := prDisplayBody(retained); got != expected {
									t.Errorf("human attribution = %q, want %q", got, expected)
								}
								if got := prDisplayBody(expected); got != expected {
									t.Errorf("published human attribution changed on reuse: %q", got)
								}
								for _, tc := range []struct{ input, want string }{{input, ""}, {retained, expected}} {
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
											t.Errorf("recorded value = %q, want %q", value, tc.want)
										}
									}
								}
								if got := stripReviewAIAttribution(input, nil); got != input {
									t.Errorf("commit filter changed %q to %q", input, got)
								}
							})
						}
					}
				}
			}
		}
	}
}

func TestPRSpanningCoauthorWrapper(t *testing.T) {
	for _, wrapper := range [][2]string{{"**", "**"}, {"`", "`"}, {"<code>", "</code>"}, {"*", "*"}, {"~~", "~~"}, {"**`", "`.**"}} {
		for _, key := range []string{"Co-Authored-By:", "co-author (quoted):"} {
			for _, separator := range []string{" ", "\n "} {
				ai := wrapper[0] + key + " Codex" + wrapper[1] + separator + "<ai@example.com>"
				human := wrapper[0] + key + " Jane" + wrapper[1] + separator + "<jane@example.com>"
				want := wrapper[0] + "co-author (quoted): Jane" + wrapper[1] + separator + "<jane@example.com>"
				for _, escaped := range []bool{false, true} {
					input, retained, expected := ai, human, want
					if escaped {
						input, retained, expected = html.EscapeString(input), html.EscapeString(retained), html.EscapeString(expected)
					}
					t.Run(input, func(t *testing.T) {
						if got := prDisplayBody(input); got != "" {
							t.Errorf("AI attribution survived: %q", got)
						}
						if got := prDisplayNoticeText(input); got != prAIAttributionNotice {
							t.Errorf("evidence attribution = %q", got)
						}
						if got := neutralizeAIAttribution(input); got != prAIAttributionNotice {
							t.Errorf("neutralized attribution = %q", got)
						}
						if got := prDisplayBody(retained); got != expected {
							t.Errorf("human attribution = %q, want %q", got, expected)
						}
					})
				}
			}
		}
	}
}

func TestPRNestedPunctuationClassificationIsIdempotent(t *testing.T) {
	for _, input := range []string{
		"**`Co-Authored-By: Codex <ai@example.com>`.**",
		"~~!<code>?`co-author (quoted): Codex <ai@example.com>`.!</code>;~~?",
		"!**?`Co-Authored-By:`.**; !~~?`Codex`.~~; !<code>?`<ai@example.com>`.</code>;",
	} {
		for _, raw := range []string{input, html.EscapeString(input)} {
			view, _ := publishedAttributionView(raw)
			key, value, ok := strings.Cut(view, ":")
			if !ok || key != "Co-Authored-By" || strings.TrimSpace(value) != "Codex <ai@example.com>" {
				t.Errorf("classification = %q for %q", view, raw)
			}
			if again, _ := publishedAttributionView(view); again != view {
				t.Errorf("classification changed on reuse: %q -> %q", view, again)
			}
		}
	}
}
