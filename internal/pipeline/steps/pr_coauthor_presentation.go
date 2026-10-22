package steps

import (
	"html"
	"regexp"
	"strings"
)

var prCoauthorContent = regexp.MustCompile(`(?i)\bco-authored-by[\t ]*:[\t ]*((?:[^<>\r\n]|\r?\n(?:[\t >"'\x60*\-]|&gt;|&#34;|&#39;)+)*?(?:<[^>\r\n]+>|&lt;[^\r\n]*?&gt;))`)

// presentPRCoauthors distinguishes quoted evidence from commit attribution.
// The name and address remain visible, including their existing escaping;
// only the field label changes. Run before budgeting or stamping an appendix,
// since the descriptive label is longer. This never changes the source
// summary, audit findings, captured evidence files, or commit messages.
func presentPRCoauthors(text string) string {
	return prCoauthorContent.ReplaceAllStringFunc(text, func(content string) string {
		// Reuse the Review classifier rather than relabeling an AI identity as
		// human. Decode only the classification copy, not the published text.
		if stripReviewAIAttribution(html.UnescapeString(content), nil) == "" {
			return ""
		}
		_, identity, _ := strings.Cut(content, ":")
		return "preserved human co-author: " + strings.TrimLeft(identity, "\t ")
	})
}
