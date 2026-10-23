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

func TestPRPayloadSpanPreservesSurroundingProse(t *testing.T) {
	const prose = `Ordinary prose mentions "co-author (quoted): Codex <ai@example.com>" as a field.`
	const link = `[Example "co-author (quoted): Codex <ai@example.com>"](https://example.com)`
	const disclosure = "Disclosure: fixtures were generated with the repository recorder."
	for name, input := range map[string]string{
		"quoted-prose-with-comma":    `"co-author (quoted): Codex <ai@example.com>", he said.`,
		"same-line-array-then-prose": `["ordinary"] ` + prose,
		"json-shaped-markdown-link":  `["co-author (quoted): Codex <ai@example.com>"](https://example.com)`,
		"array-then-prose":           "[\n \"ordinary\"\n]\n\n" + prose,
		"object-then-prose":          "{\n \"ordinary\": true\n}\n\n" + prose,
		"markdown-link":              link,
		"array-then-markdown":        "[\n \"ordinary\"\n]\n\n" + link,
		"array-then-disclosure":      "[\n \"ordinary\"\n]\n\n" + disclosure,
		"fenced-then-prose":          "```json\n[\n \"ordinary\"\n]\n```\n\n" + prose,
		"quoted-prose-after-json":    "[\n \"ordinary\"\n]\n\n\"Ordinary prose mentions co-author (quoted): Codex.\"",
	} {
		t.Run(name, func(t *testing.T) {
			for mode, display := range map[string]func(string) string{
				"body":   func(text string) string { return prDisplayBody(text) },
				"notice": func(text string) string { return prDisplayNoticeText(text) },
			} {
				if got := display(input); got != input {
					t.Errorf("%s altered prose outside a payload:\ngot %q\nwant %q", mode, got, input)
				}
			}
		})
	}
}

func TestPRPayloadSpanFiltersOnlyRecordedValues(t *testing.T) {
	const ai = `"<code>co-author (quoted): <code>Codex</code> <payload-ai@example.com></code>"`
	const human = `"Co-Authored-By: Ada Lovelace <ada@example.com>"`
	const labelled = `"co-author (quoted): Ada Lovelace \u003cada@example.com\u003e"`
	const prose = `Ordinary prose mentions "co-author (quoted): Codex <ai@example.com>" as a field.`
	const disclosure = "Disclosure: fixtures were generated with the repository recorder."
	for name, payload := range map[string]string{
		"complete":      "{\r\n \"ai\" : " + ai + ",\r\n \"human\": " + human + ",\r\n \"escape\": \"A\\u0042\\/C\"\r\n}",
		"truncated":     "{\r\n \"ai\" : " + ai + ",\r\n... [20000 bytes truncated] ...\r\n \"human\": " + human + ",\r\n \"escape\": \"A\\u0042\\/C\"\r\n}",
		"cutoff-string": "{\n \"large\": \"cutoff }\n... [20000 bytes truncated] ...\ncontinued text\", \"ai\": " + ai + ", \"human\": " + human + ", \"escape\": \"A\\u0042\\/C\"\n}",
	} {
		t.Run(name, func(t *testing.T) {
			input := payload + "\n\n" + prose + "\n" + disclosure
			want := strings.Replace(strings.Replace(input, ai, `""`, 1), human, labelled, 1)
			if got := prDisplayBody(input); got != want {
				t.Errorf("publication changed bytes outside recorded values:\ngot %q\nwant %q", got, want)
			}
		})
	}
}

func TestPRPayloadSpanPublicationPaths(t *testing.T) {
	const prose = `Ordinary prose mentions "co-author (quoted): Codex <ai@example.com>" as a field.`
	const link = `[Example "co-author (quoted): Codex <ai@example.com>"](https://example.com)`
	const disclosure = "Disclosure: fixtures were generated with the repository recorder."
	const payload = "{\n \"description\": \"<code>co-author (quoted): <code>Codex</code> <payload-ai@example.com></code>\",\n... [20000 bytes truncated] ...\n \"human\": \"Co-Authored-By: Ada Lovelace <ada@example.com>\",\n \"escape\": \"A\\u0042\\/C\"\n}"
	raw := "[\n \"ordinary\"\n]\n\n" + prose + "\n" + link + "\n" + disclosure + "\n\n" + payload + "\n\n" + prose
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
					path := filepath.Join(root, "payload.txt")
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
						content, err := composeOwnedPRContent(parts, "fix: preserve prose", block, 0)
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
					if strings.Contains(body, "payload-ai@example.com") || strings.Contains(body, "<code>Codex</code>") {
						t.Errorf("recorded AI identity survived: %s", body)
					}
					for _, want := range []string{prose, link, disclosure, "co-author (quoted): Ada Lovelace", "ada@example.com", `A\u0042\/C`, "... [20000 bytes truncated] ..."} {
						if !strings.Contains(body, want) {
							t.Errorf("publication lost %q: %s", want, body)
						}
					}
					if strings.Count(body, prose) != 2 {
						t.Errorf("prose before or after a payload changed: %s", body)
					}
				})
			}
		})
	}
}
