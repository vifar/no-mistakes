package steps

import (
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Oracle: the issue's published-body contract. The published body carries no
// AI-named co-author in any form and no raw Co-Authored-By: line, and it shows a
// human co-author as the labelled field with name and address. A plausible wrong
// behaviour is publishing the recorded line verbatim because it is not a
// complete JSON document (evidence cut from the middle) or because a folded
// trailer was classified one physical line at a time.
func TestPRDisplayTruncatedPayloadLines(t *testing.T) {
	const disclosure = "Disclosure: fixtures were generated with the repository recorder."
	for _, tc := range []struct {
		name, input string
		absent      []string
		present     []string
	}{
		{
			name:   "ai member value",
			input:  "{\"status\":\"ok\",\n \"description\": \"co-author (quoted): **Codex** <payload-ai@example.com>\",",
			absent: []string{"Codex", "payload-ai@example.com"},
			present: []string{
				"{\"status\":\"ok\",\n \"description\": ",
			},
		},
		{
			name:    "ai raw bare element",
			input:   "... [4096 bytes truncated] ...\n\n \"Co-Authored-By: Codex <ai@example.com>\",\n \"kept\"]",
			absent:  []string{"Codex", "ai@example.com", "Co-Authored-By"},
			present: []string{"... [4096 bytes truncated] ...", "\"kept\"]"},
		},
		{
			name:    "ai identity as key",
			input:   "{\"status\":\"ok\",\n \"Co-Authored-By: Codex <ai@example.com>\": \"value\",",
			absent:  []string{"Codex", "ai@example.com"},
			present: []string{"\"value\","},
		},
		{
			name:    "human member value",
			input:   "{\"status\":\"ok\",\n \"description\": \"Co-Authored-By: Ada Lovelace <ada@example.com>\",",
			absent:  []string{"Co-Authored-By"},
			present: []string{"co-author (quoted): Ada Lovelace ", "ada@example.com"},
		},
		{
			name:    "human raw bare element",
			input:   " \"Co-Authored-By: Ada Lovelace <ada@example.com>\",\n \"kept\"]",
			absent:  []string{"Co-Authored-By"},
			present: []string{"co-author (quoted): Ada Lovelace ", "ada@example.com", "\"kept\"]"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for mode, render := range map[string]func(string) string{
				"body":   func(text string) string { return prDisplayBody(text) },
				"notice": func(text string) string { return prDisplayNoticeText(text) },
			} {
				got := render(tc.input + "\n" + disclosure)
				for _, text := range tc.absent {
					if strings.Contains(got, text) {
						t.Errorf("%s published %q: %q", mode, text, got)
					}
				}
				for _, text := range tc.present {
					if !strings.Contains(got, text) {
						t.Errorf("%s lost %q: %q", mode, text, got)
					}
				}
				if !strings.HasSuffix(got, "\n"+disclosure) {
					t.Errorf("%s changed the required disclosure: %q", mode, got)
				}
			}
		})
	}
}

func TestPRDisplayFoldedTrailerPairs(t *testing.T) {
	for _, tc := range []struct {
		name, input string
		absent      []string
		present     []string
	}{
		{"ai pair", "Co-Authored-By: Claude\n <noreply@anthropic.com>", []string{"Claude", "noreply@anthropic.com", "Co-Authored-By"}, nil},
		{"ai pair indented with tab", "Co-Authored-By: gpt-5.5\n\t<model@example.com>", []string{"gpt-5.5", "model@example.com", "Co-Authored-By"}, nil},
		{"human pair", "Co-Authored-By: Jane Doe\n <jane@example.com>", []string{"Co-Authored-By"}, []string{PreservedHumanCoauthorLabel + " Jane Doe", "<jane@example.com>"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			text := "Summary line\n" + tc.input + "\nNext paragraph."
			for mode, render := range map[string]func(string) string{
				"body":   func(text string) string { return prDisplayBody(text) },
				"notice": func(text string) string { return prDisplayNoticeText(text) },
			} {
				got := render(text)
				if !strings.HasPrefix(got, "Summary line\n") || !strings.HasSuffix(got, "\nNext paragraph.") {
					t.Errorf("%s changed the surrounding text: %q", mode, got)
				}
				for _, line := range strings.Split(got, "\n") {
					if strings.HasPrefix(strings.ToLower(strings.TrimSpace(line)), "co-authored-by") {
						t.Errorf("%s published a raw trailer line %q", mode, line)
					}
				}
				for _, absent := range tc.absent {
					if strings.Contains(got, absent) {
						t.Errorf("%s published %q: %q", mode, absent, got)
					}
				}
				for _, present := range tc.present {
					if !strings.Contains(got, present) {
						t.Errorf("%s lost %q: %q", mode, present, got)
					}
				}
			}
		})
	}
	got := prDisplayFindings(types.Findings{Summary: "Co-Authored-By: Claude\n <noreply@anthropic.com>"}).Summary
	if strings.Contains(got, "Claude") || strings.Contains(got, "noreply@anthropic.com") {
		t.Errorf("finding summary published a folded AI pair: %q", got)
	}
}

// The commit filter is the base behaviour: it strips only the session keys and
// identities the base recognised, so a Pi/Cursor/Grok session trailer a human
// wrote survives in the commit body byte for byte.
func TestReviewFixCommitBodyKeepsSessionTrailersTheBaseKept(t *testing.T) {
	t.Parallel()
	const kept = "Pi-Session: abc\nCursor-Session: def\nGrok-4-Session: ghi"
	_, body := reviewFixCommitSummary("Bind receipts\n"+kept+"\nClaude-Session: gone\nCodex-Session: gone", nil, nil)
	if !strings.Contains(body, kept) {
		t.Errorf("commit body dropped a session trailer the base preserved: %q", body)
	}
	if strings.Contains(body, "gone") {
		t.Errorf("commit body kept a session trailer the base stripped: %q", body)
	}
}
