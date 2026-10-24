package steps

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestPRRecordedInlineClaimsPreserveUnrelatedBytes(t *testing.T) {
	for _, tt := range []struct {
		name string
		raw  string
		want string
	}{
		{"AI", `"Recorded risk Co-Authored-By: Codex \u003cbot@example.invalid\u003e"`, `"Recorded risk <no-mistakes: AI attribution line neutralized>"`},
		{"fenced AI", "\"```text\\nRecorded risk Co-Authored-By: Codex \\u003cbot@example.invalid\\u003e\\n```\"", "\"```text\\nRecorded risk <no-mistakes: AI attribution line neutralized>\\n```\""},
		{"fenced human", `"~~~text\nRecorded helper Co-Authored-By: Ada \u003cada@example.com\u003e\n~~~"`, `"~~~text\nRecorded helper co-author (quoted): Ada <ada@example.com>\n~~~"`},
		{"human", `"Recorded risk Co-Authored-By: Ada \u003cada@example.com\u003e."`, `"Recorded risk co-author (quoted): Ada <ada@example.com>."`},
		{"field", `"The field \"Co-Authored-By:\" identifies Ada \u003cada@example.com\u003e"`, `"The field \"Co-Authored-By:\" identifies Ada \u003cada@example.com\u003e"`},
		{"code", "\"Recorded `Co-Authored-By: Codex \\u003cbot@example.invalid\\u003e`\"", "\"Recorded `Co-Authored-By: Codex \\u003cbot@example.invalid\\u003e`\""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			for _, fragment := range []bool{false, true} {
				prefix, suffix := "{\r\n \"summary\": ", ",\r\n \"escape\": \"A\\u0042\\/C\", \"number\": 1.200e+3"
				if !fragment {
					suffix += "\r\n}"
				}
				for _, notice := range []bool{false, true} {
					want := tt.want
					if !notice {
						want = strings.ReplaceAll(want, prAIAttributionNotice, "")
					}
					if got := filterRecordedPayload(prefix+tt.raw+suffix, notice, fragment); got != prefix+want+suffix {
						t.Errorf("notice=%t fragment=%t: recorded output = %q, want %q", notice, fragment, got, prefix+want+suffix)
					}
				}
			}
		})
	}
}

func TestPRConservativePayloadBoundary(t *testing.T) {
	const disclosure = "Disclosure: fixtures were generated with the repository recorder."
	const mention = "The recorded field is `Co-Authored-By: Ada Lovelace <ada@example.com>`"
	// The intent: an AI-named co-author (name and address) is absent from every
	// published form, including a bare quoted element of a cut payload. A human
	// co-author and unrelated bytes stay byte-identical.
	const quoted = "\"co-author (quoted): Ada Lovelace <ada@example.com>\",\nhe said; this is only an example."
	const quotedAI = "\"co-author (quoted): Codex <ai@example.com>\",\nhe said; this is only an example."
	const prose = `Ordinary prose mentions "co-author (quoted): Codex <ai@example.com>" as a field.`
	const link = `[Example "co-author (quoted): Codex <ai@example.com>"](https://example.com)`
	const truncated = "{\"status\":\"ok\",\n \"description\": \"kept evidence\","
	for name, input := range map[string]string{
		"quoted-comma-prose":        quoted,
		"human-trailer-mention":     mention,
		"field-name-mention":        `The field is "Co-Authored-By:" and has no value.`,
		"prose-after-array":         "[\n \"ordinary\"\n]\n\n" + prose,
		"markdown-link":             link,
		"disclosure-after-evidence": "```json\n{\"status\":\"ok\"}\n```\n" + disclosure,
		"truncated-payload":         truncated,
	} {
		t.Run(name, func(t *testing.T) {
			for mode, render := range map[string]func(string) string{
				"body":          func(text string) string { return prDisplayBody(text) },
				"notice":        func(text string) string { return prDisplayNoticeText(text) },
				"rendered-body": func(text string) string { return neutralizeAttestationMarkers(scm.ProviderGitHub, prDisplayBody(text)) },
			} {
				if got := render(input); got != input {
					t.Errorf("%s changed the latest ruling's preserved text:\ngot %q\nwant %q", mode, got, input)
				}
			}
		})
	}
	for mode, render := range map[string]func(string) string{
		"body":   func(text string) string { return prDisplayBody(text) },
		"notice": func(text string) string { return prDisplayNoticeText(text) },
	} {
		got := render(quotedAI)
		if strings.Contains(got, "Codex") || strings.Contains(got, "ai@example.com") || !strings.HasSuffix(got, "\nhe said; this is only an example.") {
			t.Errorf("%s published an AI co-author from a cut payload element or changed the bytes after it: %q", mode, got)
		}
		got = render(truncated + "\n" + fallbackAgentFooter + "\n" + disclosure)
		assertFallbackAgentFooterAbsent(t, got)
		if !strings.HasPrefix(got, truncated+"\n") || !strings.HasSuffix(got, "\n"+disclosure) {
			t.Errorf("%s changed truncated evidence or disclosure: %q", mode, got)
		}
	}
	const ai = `"<code>co-author (quoted): <code>Codex</code> <payload-ai@example.com></code>"`
	const human = `"Co-Authored-By: Ada Lovelace <ada@example.com>"`
	const labelled = `"co-author (quoted): Ada Lovelace <ada@example.com>"`
	const document = "{\r\n \"items\": [" + ai + ", {\"human\": " + human + "}],\r\n \"escape\": \"A\\u0042\\/C\", \"number\": 1.200e+3\r\n}"
	want := strings.Replace(strings.Replace(document, ai, `""`, 1), human, labelled, 1)
	if got := prDisplayBody(document); got != want {
		t.Errorf("complete JSON changed unrelated lexical bytes:\ngot %q\nwant %q", got, want)
	}
}

func TestPRConservativePayloadPublicationPaths(t *testing.T) {
	const disclosure = "Disclosure: fixtures were generated with the repository recorder."
	const mention = "The recorded field is `Co-Authored-By: Ada Lovelace <ada@example.com>`"
	// The intent: an AI-named co-author (name and address) is absent from every
	// published form, including a bare quoted element of a cut payload. A human
	// co-author and unrelated bytes stay byte-identical.
	const quoted = "\"co-author (quoted): Ada Lovelace <ada@example.com>\",\nhe said; this is only an example."
	const quotedAI = "\"co-author (quoted): Codex <ai@example.com>\",\nhe said; this is only an example."
	const link = `[Example "co-author (quoted): Codex <ai@example.com>"](https://example.com)`
	const truncated = "{\"status\":\"ok\",\n \"description\": \"kept evidence\","
	raw := "[\n \"ordinary\"\n]\n\n" + mention + "\n" + quoted + "\n" + quotedAI + "\n" + link + "\n\n" + truncated + "\n" + fallbackAgentFooter + "\n" + disclosure
	for _, provider := range []scm.Provider{scm.ProviderGitHub, scm.ProviderBitbucket} {
		t.Run(string(provider), func(t *testing.T) {
			appendix := compliantPipelineBody(t, testPipelineHeadSHA)
			sctx := &pipeline.StepContext{Config: &config.Config{}}
			paths := map[string]func(*testing.T) string{
				"display": func(t *testing.T) string { return prDisplayText(raw) },
				"notice":  func(t *testing.T) string { return prDisplayNoticeText(raw) },
				"caption": func(t *testing.T) string {
					return prDisplayFindings(types.Findings{Artifacts: []types.TestArtifact{{Label: raw}}}).Artifacts[0].Label
				},
				"content": func(t *testing.T) string {
					return prDisplayFindings(types.Findings{Artifacts: []types.TestArtifact{{Content: raw}}}).Artifacts[0].Content
				},
				"captured": func(t *testing.T) string {
					root := t.TempDir()
					path := filepath.Join(root, "capture.txt")
					if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
						t.Fatal(err)
					}
					text, ok := embeddedArtifactText(types.TestArtifact{Path: path}, testingSummaryOptions{evidenceRoot: root}, &testingArtifactRenderState{remainingEmbeddedBytes: maxEmbeddedArtifactsTotalBytes})
					if !ok {
						t.Fatal("captured evidence omitted")
					}
					data, err := os.ReadFile(path)
					if err != nil || string(data) != raw {
						t.Fatalf("captured source changed: %v", err)
					}
					return text
				},
				"draft": func(t *testing.T) string {
					return assembleDraftPRBody(sctx, raw, "Low risk", "", appendix, 0, provider)
				},
				"narrative": func(t *testing.T) string { return assemblePRBody(sctx, raw, "Low risk", "", appendix, 0, provider) },
				"legacy-restamp": func(t *testing.T) string {
					body, rebound, err := rebindOwnedPRAttestation(appendix+"\n"+raw, strings.Repeat("b", 40), nil, pipelineAttestationPolicy{}, provider)
					if err != nil || !rebound {
						t.Fatalf("restamp: %t, %v", rebound, err)
					}
					return body
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
							block += "\n" + raw
						}
						if restamp {
							body, rebound, err := rebindOwnedPRAttestation(parts.before+"\n\n"+wrapPRAppendix(block)+"\n\n"+parts.after, strings.Repeat("b", 40), nil, pipelineAttestationPolicy{}, provider)
							if err != nil || !rebound {
								t.Fatalf("restamp: %t, %v", rebound, err)
							}
							return body
						}
						content, err := composeOwnedPRContent(parts, "fix: preserve recorded context", block, 0)
						if err != nil {
							t.Fatal(err)
						}
						return content.Body
					}
				}
			}
			for name, publish := range paths {
				t.Run(name, func(t *testing.T) {
					body := publish(t)
					assertFallbackAgentFooterAbsent(t, body)
					if strings.Contains(body, "Codex <ai@example.com>\",") {
						t.Errorf("publication carried an AI co-author: %s", body)
					}
					for _, want := range []string{mention, quoted, link, truncated, disclosure} {
						if !strings.Contains(body, want) {
							t.Errorf("publication changed preserved text %q: %s", want, body)
						}
					}
				})
			}
		})
	}
}
