package steps

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/agent"
)

// Strip even quoted attribution before normalizing summary whitespace: once
// flattened into a subject, a trailer can no longer be removed line by line.
var reviewAIAttribution = regexp.MustCompile(`(?i)^[\s>"'\x60*-]*(?:generated\s+with\b|(?:claude|codex|chatgpt|openai|anthropic|copilot|gemini|grok|pi|omp|opencode|antigravity|rovodev|acp|no-mistakes)-session\s*:|(?:co-authored-by|assisted-by)\s*:\s*(?:no-mistakes(?:\s|:|$)|(?:claude(?: code)?|codex|chatgpt|openai|anthropic|(?:github )?copilot|gemini|grok|pi|omp|opencode|antigravity|rovodev|acp)(?:\[bot\])?(?:\s*<|:|[\s>"'\x60]*$)))`)

func stripReviewAIAttribution(text string) string {
	var kept []string
	for _, line := range strings.Split(text, "\n") {
		if !reviewAIAttribution.MatchString(line) {
			kept = append(kept, line)
		}
	}
	return strings.TrimSpace(strings.Join(kept, "\n"))
}

// This is also the FixSummary returned to the executor for step_rounds. The
// record is inserted after Execute returns, so reading a prior DB row here
// would attach the wrong round's rationale to the commit.
func reviewFixCommitSummary(summary string, producer *agent.Result) (subject, body string) {
	raw := summary
	reason := "fixer returned no summary"
	if producer != nil && producer.Output != nil {
		var report commitSummary
		if err := json.Unmarshal(producer.Output, &report); err != nil {
			raw = ""
			reason = "fixer returned no readable structured summary"
		} else {
			raw = report.Summary
		}
	}
	body = stripReviewAIAttribution(raw)
	if body == "" {
		if strings.TrimSpace(raw) != "" {
			reason = "summary contained only attribution"
		}
		return "", "body: unavailable - " + reason
	}
	// Keep the existing subject's normalization and punctuation behavior.
	subject = strings.Trim(strings.Join(strings.Fields(body), " "), " \t\r\n\"'.;:,-")
	return subject, body
}

func reviewFixCommitBody(summary, previousFindings string) string {
	var b strings.Builder
	b.WriteString(summary)
	var findings Findings
	if err := json.Unmarshal([]byte(previousFindings), &findings); err != nil {
		b.WriteString("\n\nRound findings unavailable (payload could not be read).")
		return b.String()
	}
	if len(findings.Items) == 0 {
		return b.String()
	}
	b.WriteString("\n\nRound findings:")
	const maxFindings = 5
	for i, finding := range findings.Items {
		if i == maxFindings {
			fmt.Fprintf(&b, "\n- %d more findings omitted", len(findings.Items)-i)
			break
		}
		text := stripReviewAIAttribution(finding.Description)
		if file := stripReviewAIAttribution(finding.File); file != "" {
			text = file + ": " + text
		}
		text = strings.Join(strings.Fields(text), " ")
		if text == "" {
			text = "finding description unavailable"
		}
		if runes := []rune(text); len(runes) > 240 {
			text = string(runes[:237]) + "..."
		}
		b.WriteString("\n- " + text)
	}
	return b.String()
}
