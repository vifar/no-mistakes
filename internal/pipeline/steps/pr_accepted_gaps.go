package steps

import (
	"crypto/sha256"
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/safepath"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// acceptedGapsHeading opens the owner-accepted Test gap block. The block sits
// in the Pipeline section's fixed header (see splitPipelineSectionHeader), not
// in the Testing section: Testing is the first section the body budget drops,
// and an accepted gap is audit evidence that must survive every trimming path.
const acceptedGapsHeading = "### Accepted gaps"

const acceptedGapsStart = "<!-- no-mistakes:accepted-gaps:start -->"
const acceptedGapsEnd = "<!-- no-mistakes:accepted-gaps:end -->"

const maxAcceptedGapDescriptionRunes = 500

// pipelineAttestationAcceptedGaps is the machine-readable form of an accepted
// Test gap, carried on the Test step of the attestation.
type pipelineAttestationAcceptedGaps struct {
	FindingIDs      []string `json:"finding_ids"`
	ReasonDigest    string   `json:"reason_sha256"`
	CoveredByDigest string   `json:"covered_by_sha256"`
}

func attestedAcceptedGaps(sr *db.StepResult) *pipelineAttestationAcceptedGaps {
	if sr == nil || sr.StepName != types.StepTest {
		return nil
	}
	gaps, err := sr.RecordedAcceptedGaps()
	if err != nil || gaps == nil {
		return nil
	}
	return &pipelineAttestationAcceptedGaps{FindingIDs: gaps.FindingIDs, ReasonDigest: acceptedGapTextDigest(gaps.Reason), CoveredByDigest: acceptedGapTextDigest(gaps.CoveredBy)}
}

// canonicalAcceptedGapText redacts the reason and covering tests before both
// digesting and rendering them, so the digest never binds text publication
// would redact. Redaction is idempotent, so later whole-body redaction cannot
// change it. Rendering also escapes text and neutralizes reserved markers and
// closing references; those presentation rewrites are not part of the digest.
func canonicalAcceptedGapText(text string) string {
	return safepath.RedactText(text)
}

func acceptedGapTextDigest(text string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(canonicalAcceptedGapText(text))))
}

func acceptedGapAuditBlock(pipelineMD string) string {
	start := strings.Index(pipelineMD, acceptedGapsStart)
	if start < 0 {
		return ""
	}
	end := strings.Index(pipelineMD[start:], acceptedGapsEnd)
	if end < 0 {
		return ""
	}
	return pipelineMD[start : start+end+len(acceptedGapsEnd)]
}

func validateAcceptedGapAuditBudget(pipelineMD string, limit int) error {
	tail := joinBlocks(noMistakesPRSignature, carriedAttestation(pipelineMD), acceptedGapAuditBlock(pipelineMD))
	if len(tail) > maxPullRequestBodyBytes || (limit > 0 && scm.PRBodyLen(tail) > limit) {
		return fmt.Errorf("mandatory pipeline attestation and accepted-gap audit block exceed PR body limit; refusing publication")
	}
	return nil
}

func preserveAcceptedGapAudit(body, prefix, pipelineMD string, limit int, units func(string) int, clamp func(string, int) string) string {
	block := acceptedGapAuditBlock(pipelineMD)
	if block == "" {
		return body
	}
	marker := carriedAttestation(pipelineMD)
	if strings.Contains(body, block) && (marker == "" || strings.Contains(body, marker)) && (limit <= 0 || units(body) <= limit) {
		return body
	}
	tail := joinBlocks(noMistakesPRSignature, marker, block)
	if limit <= 0 {
		return joinBlocks(body, block)
	}
	if units(tail) > limit {
		return tail
	}
	return shrinkMeasuredKeepingTail(joinBlocks(prefix, tail), tail, limit, units, clamp)
}

// buildAcceptedGapsBlock renders each accepted finding with its description,
// plus the approval reason and the covering tests verbatim. It returns "" when
// no completed Test step carries a recorded gap.
func buildAcceptedGapsBlock(steps []*db.StepResult, provider scm.Provider) string {
	flavor := prBodyFlavorFor(provider)
	for _, sr := range steps {
		if sr == nil || sr.StepName != types.StepTest || sr.Status != types.StepStatusCompleted {
			continue
		}
		gaps, err := sr.RecordedAcceptedGaps()
		if err != nil || gaps == nil || len(gaps.FindingIDs) == 0 {
			continue
		}
		descriptions := make(map[string]string, len(gaps.Findings))
		for _, f := range gaps.Findings {
			// The description embeds analyzer-provided scenario names, so it
			// crosses the same publication redaction boundary as the reason.
			// A restamp writes this block through UpdatePR without the draft
			// path's whole-body redaction, so it must already be redacted.
			descriptions[f.ID] = canonicalAcceptedGapText(f.Description)
		}
		var b strings.Builder
		b.WriteString(acceptedGapsHeading)
		b.WriteString("\n")
		for _, id := range gaps.FindingIDs {
			b.WriteString("- `")
			b.WriteString(strings.ReplaceAll(id, "`", "'"))
			b.WriteString("`")
			if description := singleLineText(descriptions[id], maxAcceptedGapDescriptionRunes); description != "" {
				b.WriteString(": ")
				b.WriteString(escapePRText(description, flavor))
			}
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "- Approved: %s\n", escapePRText(canonicalAcceptedGapText(gaps.Reason), flavor))
		fmt.Fprintf(&b, "- Covered by: %s", escapePRText(canonicalAcceptedGapText(gaps.CoveredBy), flavor))
		return acceptedGapsStart + "\n" + neutralizeAttestationMarkers(provider, b.String()) + "\n" + acceptedGapsEnd
	}
	return ""
}

func singleLineText(s string, maxRunes int) string {
	s = strings.Join(strings.Fields(s), " ")
	runes := []rune(s)
	if maxRunes > 0 && len(runes) > maxRunes {
		return string(runes[:maxRunes]) + "..."
	}
	return s
}

func rebindAcceptedGapAudit(body string, steps []*db.StepResult, provider scm.Provider) (string, error) {
	block := buildAcceptedGapsBlock(steps, provider)
	starts, ends := strings.Count(body, acceptedGapsStart), strings.Count(body, acceptedGapsEnd)
	if starts == 0 && ends == 0 {
		if block == "" {
			return body, nil
		}
		return body + "\n\n" + block, nil
	}
	start, end := strings.Index(body, acceptedGapsStart), strings.Index(body, acceptedGapsEnd)
	if starts != 1 || ends != 1 || end < start {
		return "", fmt.Errorf("ambiguous accepted-gap audit markers; refusing publication")
	}
	return body[:start] + block + body[end+len(acceptedGapsEnd):], nil
}

// acceptedGapAuditApplicable reports whether body is one of ours without an
// HTML attestation: it already carries the audit markers or the pipeline
// signature (Bitbucket bodies carry no attestation comment). Restamping adds,
// replaces or removes the audit block only in such bodies.
func acceptedGapAuditApplicable(body string) bool {
	return strings.Contains(body, acceptedGapsStart) || strings.Contains(body, acceptedGapsEnd) || strings.Contains(body, noMistakesPRSignature)
}
