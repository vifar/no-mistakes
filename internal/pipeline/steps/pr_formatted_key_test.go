package steps

import (
	"encoding/json"
	"html"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestPRFormattedKeyPublicationPaths(t *testing.T) {
	const disclosure = "Disclosure: fixtures were generated with the repository recorder."
	const prose = "The recorded field is `Co-Authored-By: Ada Lovelace <quoted-human@example.com>`"
	for _, key := range []string{"Co-Authored-By:", "co-author (quoted):"} {
		for _, wrapper := range []struct{ name, open, close string }{
			{"single-backtick", "`", "`"},
			{"double-backtick", "``", "``"},
			{"bold", "**", "**"},
			{"bold-underscore", "__", "__"},
			{"italic", "*", "*"},
			{"italic-underscore", "_", "_"},
			{"strike", "~~", "~~"},
			{"html", "<code>", "</code>"},
			{"nested", "**<code>`", "`</code>**"},
		} {
			for _, escaped := range []bool{false, true} {
				name := key + "/" + wrapper.name
				if escaped {
					name += "/escaped"
				}
				t.Run(name, func(t *testing.T) {
					ai := wrapper.open + key + wrapper.close + " Codex <formatted-ai@example.com>"
					human := wrapper.open + key + wrapper.close + " Ada Lovelace <human@example.com>"
					labelled := wrapper.open + "co-author (quoted):" + wrapper.close + " Ada Lovelace <human@example.com>"
					if escaped {
						ai, human, labelled = html.EscapeString(ai), html.EscapeString(human), html.EscapeString(labelled)
					}
					raw := ai + "\n\n" + human + "\n\n" + prose + "\n" + disclosure
					appendix := compliantPipelineBody(t, testPipelineHeadSHA)
					sctx := &pipeline.StepContext{Config: &config.Config{}}
					paths := map[string]func(*testing.T) string{
						"text":   func(t *testing.T) string { return prDisplayText(raw) },
						"notice": func(t *testing.T) string { return prDisplayNoticeText(raw) },
						"fields": func(t *testing.T) string {
							fields := prDisplayFindings(types.Findings{Summary: raw, Items: []types.Finding{{Description: raw, File: raw}}, Tested: []string{raw}, Artifacts: []types.TestArtifact{{Label: raw, Content: raw}}, Scenarios: []types.TestScenario{{Name: raw, Evidence: raw, Reason: raw}}})
							values := []string{fields.Summary, fields.Items[0].Description, fields.Items[0].File, fields.Tested[0], fields.Artifacts[0].Label, fields.Artifacts[0].Content, fields.Scenarios[0].Name, fields.Scenarios[0].Evidence, fields.Scenarios[0].Reason}
							for _, value := range values {
								assertFormattedKeyPublication(t, value, labelled, prose, disclosure)
							}
							return fields.Summary
						},
						"recorded": func(t *testing.T) string {
							payload, err := json.Marshal(map[string]string{"description": raw})
							if err != nil {
								t.Fatal(err)
							}
							var decoded map[string]string
							if err := json.Unmarshal([]byte(prDisplayBody(string(payload))), &decoded); err != nil {
								t.Fatal(err)
							}
							return decoded["description"]
						},
						"captured": func(t *testing.T) string {
							root := t.TempDir()
							path := filepath.Join(root, "evidence.txt")
							if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
								t.Fatal(err)
							}
							output, ok := embeddedArtifactText(types.TestArtifact{Path: path}, testingSummaryOptions{evidenceRoot: root}, &testingArtifactRenderState{remainingEmbeddedBytes: maxEmbeddedArtifactsTotalBytes})
							if !ok {
								t.Fatal("captured evidence omitted")
							}
							if data, err := os.ReadFile(path); err != nil || string(data) != raw {
								t.Fatalf("publication changed recorded source: %v", err)
							}
							return output
						},
						"narrative": func(t *testing.T) string {
							return assemblePRBody(sctx, raw, "Low risk", "", appendix, 0, scm.ProviderGitHub)
						},
						"legacy-restamp": func(t *testing.T) string {
							output, rebound, err := rebindOwnedPRAttestation(appendix+"\n\n"+raw, strings.Repeat("b", 40), nil, pipelineAttestationPolicy{}, scm.ProviderGitHub)
							if err != nil || !rebound {
								t.Fatalf("restamp: %t, %v", rebound, err)
							}
							return output
						},
					}
					for _, component := range []string{"before", "after", "appendix"} {
						for _, restamp := range []bool{false, true} {
							name := component
							if restamp {
								name += "-restamp"
							}
							paths[name] = func(t *testing.T) string {
								parts, block := prOwnedBody{}, appendix
								switch component {
								case "before":
									parts.before = raw
								case "after":
									parts.after = "\n\n" + raw
								case "appendix":
									block += "\n\n" + raw
								}
								if restamp {
									output, rebound, err := rebindOwnedPRAttestation(parts.before+"\n\n"+wrapPRAppendix(block)+"\n\n"+parts.after, strings.Repeat("b", 40), nil, pipelineAttestationPolicy{}, scm.ProviderGitHub)
									if err != nil || !rebound {
										t.Fatalf("restamp: %t, %v", rebound, err)
									}
									return output
								}
								content, err := composeOwnedPRContent(parts, "fix: preserve entries", block, 0)
								if err != nil {
									t.Fatal(err)
								}
								return content.Body
							}
						}
					}
					for name, publish := range paths {
						t.Run(name, func(t *testing.T) { assertFormattedKeyPublication(t, publish(t), labelled, prose, disclosure) })
					}
					t.Run("review-summary", func(t *testing.T) {
						_, body := reviewFixCommitSummary(raw, nil, nil)
						if strings.Contains(body, "formatted-ai@example.com") {
							t.Fatalf("Review summary retained AI entry: %s", body)
						}
						for _, want := range []string{human, prose, disclosure} {
							if !strings.Contains(body, want) {
								t.Errorf("Review commit summary changed %q: %s", want, body)
							}
						}
					})
					t.Run("title", func(t *testing.T) {
						const title = "fix: preserve repair summaries"
						if output, err := renderPRTitle(nil, title+"\n\n"+ai); err != nil || output != title {
							t.Errorf("published title = %q, %v, want %q", output, err, title)
						}
					})
				})
			}
		}
	}
}

func assertFormattedKeyPublication(t *testing.T, output, human, prose, disclosure string) {
	t.Helper()
	if strings.Contains(output, "formatted-ai@example.com") {
		t.Errorf("publication retained a formatted AI entry: %s", output)
	}
	for _, want := range []string{human, prose, disclosure} {
		if !strings.Contains(output, want) {
			t.Errorf("publication changed or lost %q: %s", want, output)
		}
	}
}

func TestPRFormattedKeyPreservesMentions(t *testing.T) {
	for name, prose := range map[string]string{
		"quoted-field-name":        `"Co-Authored-By:" is a field name.`,
		"quoted-field-example":     "\"co-author (quoted): Ada Lovelace <ada@example.com>\",\nhe said; this is only an example.",
		"inline-human-example":     "The recorded field is `Co-Authored-By: Ada Lovelace <quoted-human@example.com>`",
		"formatted-field-name":     "The field is **Co-Authored-By:** and has no value here.",
		"quoted-phrase-no-address": "`Co-Authored-By: a quoted phrase`",
		"published-label-phrase":   "`co-author (quoted): a quoted phrase`",
	} {
		t.Run(name, func(t *testing.T) {
			for mode, publish := range map[string]func(string) string{
				"body":      func(text string) string { return prDisplayBody(text) },
				"notice":    func(text string) string { return prDisplayNoticeText(text) },
				"narrative": func(text string) string { return neutralizeAttestationMarkers(scm.ProviderGitHub, text) },
			} {
				if got := publish(prose); got != prose {
					t.Errorf("%s changed a field mention: got %q, want %q", mode, got, prose)
				}
			}
		})
	}
}

func TestPRAttributionEntryRequiresAddressOrKnownIdentity(t *testing.T) {
	for _, input := range []string{"co-author (quoted): Codex", "**Co-Authored-By:** Codex", "<code>Co-Authored-By:</code> Codex"} {
		if got := prDisplayText(input); got != "" {
			t.Errorf("known AI identity survived: %q", got)
		}
	}
	for _, input := range []string{"co-author (quoted): the original author", "`Co-Authored-By: an ordinary quoted phrase`"} {
		if got := prDisplayBody(input); got != input {
			t.Errorf("no-address mention changed: got %q, want %q", got, input)
		}
	}
}

// The intent: no published line begins with "Co-Authored-By:". A mailbox-less
// line is classified by its value: an AI identity is removed, a human name is
// rendered as the labelled field with the value preserved, and ordinary prose
// and the disclosure stay byte-identical.
func TestPRMailboxlessCoAuthorLine(t *testing.T) {
	const disclosure = "Disclosure: fixtures were generated with the repository recorder."
	const prose = "This documents the Co-Authored-By: field."
	for _, tc := range []struct{ name, line, body, notice string }{
		{"human", "Co-Authored-By: Jane Doe", "co-author (quoted): Jane Doe", "co-author (quoted): Jane Doe"},
		{"indented human", "  co-authored-by:Jane Doe", "  co-author (quoted):Jane Doe", "  co-author (quoted):Jane Doe"},
		{"claude", "Co-Authored-By: Claude", "", prAIAttributionNotice},
		{"codex", "Co-Authored-By: Codex", "", prAIAttributionNotice},
		{"model", "Co-Authored-By: gpt-5.5", "", prAIAttributionNotice},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := prose + "\n" + tc.line + "\n" + disclosure
			if got, want := prDisplayBody(input), prose+"\n"+tc.body+"\n"+disclosure; got != want {
				t.Errorf("body: got %q, want %q", got, want)
			}
			if got, want := prDisplayNoticeText(input), prose+"\n"+tc.notice+"\n"+disclosure; got != want {
				t.Errorf("notice: got %q, want %q", got, want)
			}
			narrative := neutralizeAttestationMarkers(scm.ProviderGitHub, input)
			for _, line := range strings.Split(narrative, "\n") {
				if strings.HasPrefix(strings.ToLower(strings.TrimSpace(line)), "co-authored-by") && strings.TrimSpace(line) != prose {
					t.Errorf("narrative published a raw trailer line %q:\n%s", line, narrative)
				}
			}
			if strings.Contains(narrative, "Claude") || strings.Contains(narrative, "Codex") || strings.Contains(narrative, "gpt-5.5") {
				t.Errorf("narrative carries an AI-named co-author:\n%s", narrative)
			}
		})
	}
	if got := prDisplayBody("Co-Authored-By:"); got != "Co-Authored-By:" {
		t.Errorf("field name alone changed: %q", got)
	}
}
