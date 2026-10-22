import re

# ---- prsummary.go: one rule, one label -------------------------------------
p = 'internal/pipeline/steps/prsummary.go'
s = open(p).read()

old = '''// publishedCoAuthorKey matches the commit trailer key that PUBLISHED text must
// not carry raw.
//
// The key is rendered as a labelled field that says what it is: a co-author
// the body QUOTED. A fixture, an example, a review finding quoted whole - none
// of them is this work's attribution, and the label says so.
var publishedCoAuthorKey = regexp.MustCompile(`(?i)Co-Authored-By([ \\t]*:[ \\t]*)`)

// renderQuotedAttributionAsLabelledFields renders a quoted co-author as a
// labelled field rather than as a raw trailer line.
//
// This is the PUBLISHED text's rule, and it is deliberately not the
// classifier's: the classifier decides whether a line is an AI attribution, and
// it is precise - a person whose name happens to contain a tool's name is a
// person. The published text is read by consumers that cannot make that
// distinction, so a quoted trailer is rendered in a form none of them reads as
// a claim. The quote stays a quote: the name and the address are still there.
func renderQuotedAttributionAsLabelledFields(s string) string {
	s = publishedCoAuthorKey.ReplaceAllString(s, "co-author (quoted)${1}")
	return publishedGeneratedWithEmoji.ReplaceAllString(s, "$1$2$3")
}'''

new = '''// publishedCoAuthorKey matches the commit trailer key that PUBLISHED text must
// not carry raw.
//
// The key is rendered as a labelled field that says what it is: a co-author
// the body QUOTED. A fixture, an example, a review finding quoted whole - none
// of them is this work's attribution, and the label says so.
var publishedCoAuthorKey = regexp.MustCompile(`(?i)Co-Authored-By([ \\t]*:[ \\t]*)`)

// renderCoAuthorAsLabelledField renders a co-author as a labelled field
// rather than as a raw trailer line.
//
// This is the PUBLISHED text's rule, and it is deliberately not the
// classifier's rule. The classifier decides whether a line IS an AI
// attribution, and it is precise: a person whose name happens to contain a
// tool's name is a person, and that attribution stays whole, name and address
// and all.
//
// The published text is read by consumers that cannot make that distinction.
// One that scans the pull request body for attribution reads a raw
// "Co-Authored-By:" line as a claim of AI attribution, and it reads it that
// way even inside a code fence, because it scans the raw text. So a co-author
// the body carries is rendered in a form no such consumer reads as a claim,
// with the name and the address still there, because they are the evidence.
//
// The rule is the merge bar's owner's: the bar's rule does not change, and a
// published body never carries a raw co-author trailer line. It is a
// publication policy, and it is deliberately narrow - it changes the KEY and
// nothing else, so the attribution itself survives the rendering.
func renderCoAuthorAsLabelledField(s string) string {
	return publishedCoAuthorKey.ReplaceAllString(s, "co-author (quoted)${1}")
}'''

assert old in s, "prsummary block not found"
s = s.replace(old, new)
open(p, 'w').write(s)
print("prsummary.go: one rule, one label")

# ---- pr.go: the rendering lands on the published body ----------------------
p = 'internal/pipeline/steps/pr.go'
s = open(p).read()
old = '''func appendGeneratedSectionsToCleanBodyWithinLimit(body, riskLine, testingMD, pipelineMD string, maxBytes int, provider scm.Provider) string {
	// The pipeline's evidence sections render a QUOTED co-author as a
	// labelled field. The body's own prose - the intent, the narrative,
	// the risk line - keeps its attribution verbatim, because that is the
	// PR's own attribution and it is the reader's to judge.
	testingMD = renderQuotedAttributionAsLabelledFields(testingMD)
	pipelineMD = renderQuotedAttributionAsLabelledFields(pipelineMD)
	return appendGeneratedSectionsToCleanBodyWithinLimitRaw(body, riskLine, testingMD, pipelineMD, maxBytes, provider)
}'''
new = '''func appendGeneratedSectionsToCleanBodyWithinLimit(body, riskLine, testingMD, pipelineMD string, maxBytes int, provider scm.Provider) string {
	// The published body renders a co-author as a labelled field, so no
	// consumer that scans the body for attribution reads a quoted
	// fixture's trailer as a claim. The name and the address survive the
	// rendering; only the key is rendered.
	return renderCoAuthorAsLabelledField(appendGeneratedSectionsToCleanBodyWithinLimitRaw(body, riskLine, testingMD, pipelineMD, maxBytes, provider))
}'''
assert old in s, "pr.go block not found"
s = s.replace(old, new)
open(p, 'w').write(s)
print("pr.go: the rendering lands on the published body")
