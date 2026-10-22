package steps

import (
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

// The published body must not carry a raw co-author trailer line.
//
// A consumer that scans the body for attribution - the merge bar, a reviewer -
// reads a raw "Co-Authored-By:" line as a claim of AI attribution, and it
// reads it that way even inside a code fence, because it scans the raw text.
//
// So a co-author the body QUOTES - a test fixture, an example - is rendered as
// a labelled field instead. The quote stays a quote: the name and the address
// are the evidence, and they stay.
func TestPublishedBodyRendersAQuotedCoAuthorAsALabelledField(t *testing.T) {
	body := "## Testing\n\n```text\nCo-Authored-By: Claude Smith <code@example.com>\n```\n"
	got := publishedBody(body, scm.ProviderGitHub)
	if strings.Contains(got, "Co-Authored-By:") {
		t.Fatalf("the published body carries a raw co-author trailer: %q", got)
	}
	if !strings.Contains(got, "co-author (quoted):") {
		t.Fatalf("the co-author was dropped instead of rendered as a labelled field: %q", got)
	}
	if !strings.Contains(got, "Claude Smith <code@example.com>") {
		t.Fatalf("the co-author's name and address were lost: %q", got)
	}
}

// The robot emoji is how a "Generated with" line reads as a claim, so the
// published text does not carry it either.
func TestPublishedBodyDropsTheRobotEmojiFromAQuotedFooter(t *testing.T) {
	body := "## Testing\n\n```text\n\U0001F916 Generated with A Person <person@example.com>\n```\n"
	got := publishedBody(body, scm.ProviderGitHub)
	if strings.Contains(got, "\U0001F916") {
		t.Fatalf("the published body carries the robot emoji: %q", got)
	}
	if !strings.Contains(got, "Generated with A Person <person@example.com>") {
		t.Fatalf("the footer was lost: %q", got)
	}
}

// An AI attribution is still broken, not rendered: the line is replaced by the
// neutralization notice, so the body claims nothing at all.
func TestPublishedBodyStillBreaksAnAIAttribution(t *testing.T) {
	body := "## Testing\n\n```text\nCo-Authored-By: Codex <bot@example.invalid>\n```\n"
	got := publishedBody(body, scm.ProviderGitHub)
	if strings.Contains(got, "Codex") {
		t.Fatalf("the published body still carries the AI's name: %q", got)
	}
	if !strings.Contains(got, "no-mistakes: AI attribution line neutralized") {
		t.Fatalf("the published body does not say the line was neutralized: %q", got)
	}
}

// publishedBody assembles the body the way the pipeline publishes it, so the
// test measures the text a consumer of the pull request reads.
func publishedBody(body string, provider scm.Provider) string {
	return appendGeneratedSectionsToCleanBodyWithinLimit(body, "", "", "", 64*1024, provider)
}
