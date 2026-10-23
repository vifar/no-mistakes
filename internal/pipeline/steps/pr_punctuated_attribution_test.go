package steps

import (
	"encoding/json"
	"html"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
)

func TestPRPunctuatedCoauthorPublication(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ input, want string }{
		{"co-author (quoted): Claude", ""},
		{"co-author (quoted): Claude\n <noreply@anthropic.com>", ""},
		{"co-author (quoted): Claude Smith <claude@example.com>", "co-author (quoted): Claude Smith <claude@example.com>"},
		{"Co-Authored-By: Codex <ai@example.com>.", ""},
		{"co-author (quoted): Codex <ai@example.com>.", ""},
		{"Co-Authored-By: Claude <noreply@anthropic.com>!", ""},
		{"Co-Authored-By: Codex\n <ai@example.com>.", ""},
		{"co-author (quoted): Codex\n <ai@example.com>.", ""},
		{"Co-Authored-By: **Codex** <ai@example.com>.", ""},
		{"Co-Authored-By: Codex <code><ai@example.com></code>.", ""},
		{"Co-Authored-By: Jane <jane@example.com>.", "co-author (quoted): Jane <jane@example.com>."},
		{"co-author (quoted): Jane <jane@example.com>.", "co-author (quoted): Jane <jane@example.com>."},
		{"Co-Authored-By: Jane\n <jane@example.com>.", "co-author (quoted): Jane\n <jane@example.com>."},
		{"co-author (quoted): Jane\n <jane@example.com>.", "co-author (quoted): Jane\n <jane@example.com>."},
		{"Co-Authored-By: Claude\n <claude@example.com>.", "co-author (quoted): Claude\n <claude@example.com>."},
		{"Co-Authored-By: Claude Smith <claude@example.com>?!", "co-author (quoted): Claude Smith <claude@example.com>?!"},
	} {
		for _, wrapper := range [][2]string{{"", ""}, {"> ", ""}, {"- ", ""}, {"**", "**"}, {"`", "`"}, {"<code>", "</code>"}} {
			for _, escaped := range []bool{false, true} {
				input := wrapper[0] + tc.input + wrapper[1]
				want, notice := "", prAIAttributionNotice
				if tc.want != "" {
					want = wrapper[0] + tc.want + wrapper[1]
					notice = want
				}
				if escaped {
					input = html.EscapeString(input)
					want = html.EscapeString(want)
					if tc.want != "" {
						notice = want
					}
				}
				t.Run(input, func(t *testing.T) {
					if got := prDisplayBody(input); got != want {
						t.Errorf("published attribution = %q, want %q", got, want)
					}
					if got := prDisplayNoticeText(input); got != notice {
						t.Errorf("published evidence = %q, want %q", got, notice)
					}
					neutralized := input
					if tc.want == "" {
						neutralized = prAIAttributionNotice
					}
					if got := neutralizeAIAttribution(input); got != neutralized {
						t.Errorf("neutralized attribution = %q, want %q", got, neutralized)
					}
					encoded, err := marshalJSONString(input)
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
						if value != want {
							t.Errorf("recorded attribution = %q, want %q", value, want)
						}
					}
				})
			}
		}
	}
}

func TestPRPunctuatedCoauthorComposition(t *testing.T) {
	t.Parallel()
	const fixture = "Co-Authored-By: Codex\n <ai@example.com>.\n\nCo-Authored-By: Jane\n <jane@example.com>.\n\nDisclosure: fixtures were generated with the repository recorder."
	appendix := compliantPipelineBody(t, testPipelineHeadSHA)
	for _, body := range []string{
		assemblePRBody(&pipeline.StepContext{Config: &config.Config{}}, fixture, "Low risk", "", appendix, 0, scm.ProviderGitHub),
		func() string {
			content, err := composeOwnedPRContent(prOwnedBody{before: fixture, after: fixture}, "fix: repair", appendix+"\n\n"+fixture, 0)
			if err != nil {
				t.Fatal(err)
			}
			return content.Body
		}(),
		func() string {
			body, rebound, err := rebindOwnedPRAttestation(fixture+"\n\n"+wrapPRAppendix(appendix)+"\n\n"+fixture, strings.Repeat("b", 40), nil, pipelineAttestationPolicy{}, scm.ProviderGitHub)
			if err != nil || !rebound {
				t.Fatalf("restamp = %t, %v", rebound, err)
			}
			return body
		}(),
	} {
		for _, forbidden := range []string{"Codex", "ai@example.com", "Co-Authored-By:"} {
			if strings.Contains(body, forbidden) {
				t.Errorf("published forbidden attribution %q: %s", forbidden, body)
			}
		}
		for _, retained := range []string{"co-author (quoted): Jane\n <jane@example.com>.", humanCoauthorDisclosure} {
			if !strings.Contains(body, retained) {
				t.Errorf("lost human attribution or disclosure %q: %s", retained, body)
			}
		}
	}
}

func TestPRPunctuatedAttributionKeepsCommitAndConfiguredContracts(t *testing.T) {
	t.Parallel()
	for _, input := range []string{
		"co-author (quoted): Claude",
		"Co-Authored-By: Codex <ai@example.com>.",
		"Co-Authored-By: Jane <jane@example.com>.",
		"Co-Authored-By: Claude\n <noreply@anthropic.com>.",
		"Co-Authored-By: Jane\n <jane@example.com>.",
	} {
		if got := stripReviewAIAttribution(input, nil); got != input {
			t.Errorf("commit filter changed %q to %q", input, got)
		}
	}
	const input = "Co-Authored-By: Jane <jane@example.com>."
	if got := prDisplayBody(input, "Co-Authored-By: Jane <jane@example.com>"); got != "co-author (quoted): Jane <jane@example.com>." {
		t.Errorf("configured value matched without its punctuation: %q", got)
	}
	if got := prDisplayBody(input, input); got != "" {
		t.Errorf("exact configured value survived: %q", got)
	}
}
