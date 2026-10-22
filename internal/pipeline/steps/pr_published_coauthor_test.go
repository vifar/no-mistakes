package steps

import (
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

// A co-author the body QUOTES is rendered as a labelled field.
//
// A consumer that scans the pull request body for attribution - the merge
// bar, a reviewer - reads a raw "Co-Authored-By:" line as a claim of AI
// attribution, and it reads it that way even inside a code fence, because it
// scans the raw text. So a co-author the body quotes - a test fixture, an
// example, a captured artifact - is rendered in a form no consumer reads as a
// claim.
//
// The quote stays a quote: the name and the address are the evidence, and they
// stay.
func TestPublishedBodyRendersAQuotedCoAuthorAsALabelledField(t *testing.T) {
	testing := "## Testing\n\n```text\nCo-Authored-By: Claude Smith <code@example.com>\n```\n"
	got := publishedBody("", "", testing, scm.ProviderGitHub)
	if strings.Contains(got, "Co-Authored-By:") {
		t.Fatalf("the published body carries a raw co-author trailer: %q", got)
	}
	if !strings.Contains(got, "co-author (quoted):") {
		t.Fatalf("the co-author was dropped instead of rendered as a labelled field: %q", got)
	}
	if !strings.Contains(got, "Claude Smith &lt;code@example.com&gt;") &&
		!strings.Contains(got, "Claude Smith <code@example.com>") {
		t.Fatalf("the co-author's name and address were lost: %q", got)
	}
}

// The body's OWN attribution is not touched.
//
// The intent, the narrative and the risk line are the PR's own words. A
// human attribution in them is legitimate attribution, and it stays whole -
// key, name and address - because the reader is the one who judges it.
func TestPublishedBodyKeepsItsOwnAttributionVerbatim(t *testing.T) {
	body := "## What Changed\n\nPreserve the selected command environment.\nCo-Authored-By: Claude Smith <code@example.com>\n"
	risk := "Low: Recorded risk\n"
	got := publishedBody(body, risk, "", scm.ProviderGitHub)
	if !strings.Contains(got, "Co-Authored-By: Claude Smith <code@example.com>") {
		t.Fatalf("the body's own attribution was rewritten: %q", got)
	}
}

// An AI attribution is still broken, not rendered.
//
// The classifier is unchanged: a line that IS an AI attribution is replaced by
// the neutralization notice, so the body claims nothing at all. The labelled
// field is for the lines the classifier deliberately keeps - a person's
// attribution, and an address whose form protects it from being the AI's.
func TestPublishedBodyStillBreaksAnAIAttribution(t *testing.T) {
	got := neutralizeAttestationMarkers(scm.ProviderGitHub, "Co-Authored-By: Codex <bot@example.invalid>")
	if strings.Contains(got, "Codex") {
		t.Fatalf("the published body still carries the AI's name: %q", got)
	}
	if !strings.Contains(got, "no-mistakes: AI attribution line neutralized") {
		t.Fatalf("the published body does not say the line was neutralized: %q", got)
	}
}

// An ordinary emoji is ordinary.
//
// The published text renders a quoted co-author as a labelled field; it does
// not go looking for characters and deleting them from prose. An emoji in a
// quoted line is part of the quote.
func TestPublishedBodyKeepsAnOrdinaryEmoji(t *testing.T) {
	testing := "## Testing\n\n```text\n\U0001F916 Robot integration passed\n```\n"
	got := publishedBody("", "", testing, scm.ProviderGitHub)
	if !strings.Contains(got, "\U0001F916 Robot integration passed") {
		t.Fatalf("the published body dropped an ordinary emoji: %q", got)
	}
}

// publishedBody assembles the body the way the pipeline publishes it, so the
// test measures the text a consumer of the pull request reads.
func publishedBody(body, risk, testing string, provider scm.Provider) string {
	return appendGeneratedSectionsToCleanBodyWithinLimit(body, risk, testing, "", 64*1024, provider)
}
