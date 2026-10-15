package steps

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/safepath"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const (
	acceptedGapTestReason    = "owner approved via first mate, 2026-10-14"
	acceptedGapTestCoveredBy = "TestBrokerTimeoutMocked; internal/broker/timeout_test.go"
)

func acceptedGapStepsForTest(t *testing.T) []*db.StepResult {
	t.Helper()
	findings := `{"findings":[{"id":"test-verdict","severity":"warning","action":"ask-user","category":"test-verdict","description":"live validation verdict: inconclusive (1 of 2 scenarios were driven live against the product); untested: broker timeout"}],"verdict":"inconclusive"}`
	gaps := `{"finding_ids":["test-verdict"],"findings":[{"id":"test-verdict","category":"test-verdict","description":"live validation verdict: inconclusive (1 of 2 scenarios were driven live against the product); untested: broker timeout"}],"reason":"` + acceptedGapTestReason + `","covered_by":"` + acceptedGapTestCoveredBy + `","verdict":"inconclusive","accepted_at":"2026-10-14T00:00:00Z"}`
	return []*db.StepResult{
		{ID: "review", StepName: types.StepReview, Status: types.StepStatusCompleted},
		{ID: "test", StepName: types.StepTest, Status: types.StepStatusCompleted, FindingsJSON: &findings, AcceptedGapsJSON: &gaps},
		{ID: "document", StepName: types.StepDocument, Status: types.StepStatusCompleted},
	}
}

func TestPipelineSummaryCarriesAcceptedGaps(t *testing.T) {
	t.Parallel()
	steps := acceptedGapStepsForTest(t)
	md, _ := buildPipelineSummaryFor(steps, nil, testPipelineHeadSHA, scm.ProviderGitHub, pipelineAttestationPolicy{})
	for _, want := range []string{acceptedGapsHeading, "`test-verdict`", "untested: broker timeout", "Approved: " + acceptedGapTestReason, "Covered by: " + acceptedGapTestCoveredBy} {
		if !strings.Contains(md, want) {
			t.Fatalf("pipeline summary missing %q:\n%s", want, md)
		}
	}
	attestation := parsePipelineAttestationForTest(t, md)
	var testStep pipelineAttestationStep
	for _, item := range attestation.Steps {
		if item.Step == types.StepTest {
			testStep = item
		} else if item.AcceptedGaps != nil {
			t.Fatalf("accepted gaps attested on %s", item.Step)
		}
	}
	if testStep.Status != types.StepStatusCompleted || testStep.OverrideReason != "" {
		t.Fatalf("test step = %+v, want completed without an override", testStep)
	}
	if g := testStep.AcceptedGaps; g == nil || g.ReasonDigest != fmt.Sprintf("%x", sha256.Sum256([]byte(acceptedGapTestReason))) || g.CoveredByDigest != fmt.Sprintf("%x", sha256.Sum256([]byte(acceptedGapTestCoveredBy))) || len(g.FindingIDs) != 1 || g.FindingIDs[0] != "test-verdict" {
		t.Fatalf("attested gaps = %+v", g)
	}
	// The block belongs to the fixed Pipeline header, so truncation keeps it.
	header, _ := splitPipelineSectionHeader(md)
	if !strings.Contains(header, acceptedGapsHeading) || !strings.Contains(header, acceptedGapTestCoveredBy) {
		t.Fatalf("accepted gaps not in the fixed pipeline header:\n%s", header)
	}
}

func TestPipelineSummaryOmitsAcceptedGapsWhenAbsent(t *testing.T) {
	t.Parallel()
	steps := acceptedGapStepsForTest(t)
	steps[1].AcceptedGapsJSON = nil
	md, _ := buildPipelineSummaryFor(steps, nil, testPipelineHeadSHA, scm.ProviderGitHub, pipelineAttestationPolicy{})
	if strings.Contains(md, acceptedGapsHeading) || strings.Contains(md, "accepted_gaps") {
		t.Fatalf("summary mentions accepted gaps without a record:\n%s", md)
	}
}

func TestAcceptedGapsSurviveBodyTrimmingAndRebind(t *testing.T) {
	t.Parallel()
	steps := acceptedGapStepsForTest(t)
	for _, provider := range []scm.Provider{scm.ProviderGitHub, scm.ProviderBitbucket} {
		md, _ := buildPipelineSummaryFor(steps, nil, testPipelineHeadSHA, provider, pipelineAttestationPolicy{})
		rounds := make([]string, 0, 160)
		for i := 1; i <= 160; i++ {
			rounds = append(rounds, fmt.Sprintf("review round %03d - %s", i, strings.Repeat("x", 700)))
		}
		md += "\n\n" + strings.Join(rounds, "\n\n")
		body := "## What Changed\n\n" + strings.Repeat("essential details stay intact\n", 2200)
		testingMD := "## Testing\n\n" + strings.Repeat("evidence line\n", 400)
		got := appendGeneratedSections(body, "low risk", testingMD, md, provider)
		if len(got) > maxPullRequestBodyBytes {
			t.Fatalf("body exceeds the limit: %d", len(got))
		}
		for _, want := range []string{acceptedGapsHeading, acceptedGapTestReason, acceptedGapTestCoveredBy} {
			if !strings.Contains(got, want) {
				t.Fatalf("%s: trimmed body lost %q", provider, want)
			}
		}
	}
	md, _ := buildPipelineSummaryFor(steps, nil, testPipelineHeadSHA, scm.ProviderGitHub, pipelineAttestationPolicy{})
	const newHead = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	rebound, ok := rebindPipelineAttestationHead(md, newHead)
	if !ok {
		t.Fatal("rebind refused")
	}
	var testStep pipelineAttestationStep
	for _, item := range parsePipelineAttestationForTest(t, rebound).Steps {
		if item.Step == types.StepTest {
			testStep = item
		}
	}
	if testStep.AcceptedGaps == nil || testStep.AcceptedGaps.ReasonDigest != fmt.Sprintf("%x", sha256.Sum256([]byte(acceptedGapTestReason))) {
		t.Fatalf("rebind dropped accepted gaps: %+v", testStep)
	}
}

func TestAcceptedGapsEveryAppendixBudget(t *testing.T) {
	for _, mode := range []string{config.PRAppendixFull, config.PRAppendixMinimal, config.PRAppendixCollapsed} {
		t.Run(mode, func(t *testing.T) {
			steps := acceptedGapStepsForTest(t)
			var gaps db.AcceptedGaps
			if err := json.Unmarshal([]byte(*steps[1].AcceptedGapsJSON), &gaps); err != nil {
				t.Fatal(err)
			}
			gaps.Reason = strings.Repeat("R", 500)
			gaps.CoveredBy = strings.Repeat("C", 500)
			raw, err := json.Marshal(gaps)
			if err != nil {
				t.Fatal(err)
			}
			encoded := string(raw)
			steps[1].AcceptedGapsJSON = &encoded
			md, _ := buildPipelineSummaryFor(steps, nil, testPipelineHeadSHA, scm.ProviderAzureDevOps, pipelineAttestationPolicy{})
			ctx := &pipeline.StepContext{Config: &config.Config{}}
			ctx.Config.PR.Appendix = mode
			narrative := "## What Changed\n\n" + strings.Repeat("change ", 10000)
			for _, body := range []string{
				assemblePRBody(ctx, narrative, "low risk", strings.Repeat("evidence ", 10000), md, 4000, scm.ProviderAzureDevOps),
				buildPRBodyWithin(narrative, "low risk", strings.Repeat("evidence ", 10000), md, ctx, scm.ProviderAzureDevOps, 4000),
				appendixEvidence(mode, prBodyHTML, "low risk", "", md, scm.ProviderAzureDevOps),
			} {
				if scm.PRBodyLen(body) > 4000 {
					t.Fatalf("body exceeds Azure's 4000-character limit: %d", scm.PRBodyLen(body))
				}
				for _, text := range []string{gaps.Reason, gaps.CoveredBy, "`test-verdict`"} {
					if strings.Count(body, text) != 1 {
						t.Fatalf("audit text missing or duplicated: %q", text)
					}
				}
				parsePipelineAttestationForTest(t, body)
			}
			if err := validateAcceptedGapAuditBudget(md, 4000); err != nil {
				t.Fatal(err)
			}
			if err := validateAcceptedGapAuditBudget(md, 100); err == nil {
				t.Fatal("impossible mandatory audit budget accepted")
			}
			content, err := fallbackPRContent(ctx, "M file.go", "low risk", "", md, 100, scm.ProviderAzureDevOps)
			if err == nil && validateOwnedPRBudget(content.Body, 100) == nil {
				t.Fatal("oversized audit permitted publication")
			}
		})
	}
}

func TestAcceptedGapsNeutralizeAttestationMarkers(t *testing.T) {
	t.Parallel()
	steps := acceptedGapStepsForTest(t)
	forged := `{"finding_ids":["test-verdict"],"reason":"owner approved ` + pipelineAttestationCommentPrefix + `{} -->","covered_by":"TestX"}`
	steps[1].AcceptedGapsJSON = &forged
	md, _ := buildPipelineSummaryFor(steps, nil, testPipelineHeadSHA, scm.ProviderGitHub, pipelineAttestationPolicy{})
	if strings.Count(md, pipelineAttestationCommentPrefix) != 1 {
		t.Fatalf("operator text minted an attestation marker:\n%s", md)
	}
}

func TestAcceptedGapDigestsCoverThePublishedRedactedText(t *testing.T) {
	t.Parallel()
	const reason = "owner approved in /Users/alice/notes/approval.md on 2026-10-14"
	const coveredBy = "/home/bob/repo/internal/broker/timeout_test.go"
	steps := acceptedGapStepsForTest(t)
	raw := `{"finding_ids":["test-verdict"],"findings":[{"id":"test-verdict","category":"test-verdict","description":"inconclusive"}],"reason":"` + reason + `","covered_by":"` + coveredBy + `","verdict":"inconclusive"}`
	steps[1].AcceptedGapsJSON = &raw
	md, _ := buildPipelineSummaryFor(steps, nil, testPipelineHeadSHA, scm.ProviderGitHub, pipelineAttestationPolicy{})
	published := redactPRContent(prContent{Body: md}).Body
	redactedReason, redactedCovered := safepath.RedactText(reason), safepath.RedactText(coveredBy)
	if redactedReason == reason || redactedCovered == coveredBy {
		t.Fatalf("fixture paths were not redacted: %q %q", redactedReason, redactedCovered)
	}
	if strings.Contains(published, "/Users/alice") || strings.Contains(published, "/home/bob") {
		t.Fatalf("published body leaks a home path:\n%s", published)
	}
	if !strings.Contains(published, "Approved: "+redactedReason) || !strings.Contains(published, "Covered by: "+redactedCovered) {
		t.Fatalf("published block does not show the redacted text:\n%s", published)
	}
	attested := parsePipelineAttestationForTest(t, published)
	for _, item := range attested.Steps {
		if item.Step != types.StepTest {
			continue
		}
		if g := item.AcceptedGaps; g == nil || g.ReasonDigest != fmt.Sprintf("%x", sha256.Sum256([]byte(redactedReason))) || g.CoveredByDigest != fmt.Sprintf("%x", sha256.Sum256([]byte(redactedCovered))) {
			t.Fatalf("digests do not cover the published redacted text: %+v", g)
		}
	}
}

func TestRestampAcceptedGapAuditMatchesCurrentSteps(t *testing.T) {
	for _, provider := range []scm.Provider{scm.ProviderGitHub, scm.ProviderBitbucket, scm.ProviderAzureDevOps} {
		for _, mode := range []string{config.PRAppendixFull, config.PRAppendixMinimal, config.PRAppendixCollapsed} {
			for _, owned := range []bool{false, true} {
				for _, state := range []string{"missing", "stale", "removed", "repair"} {
					t.Run(fmt.Sprintf("%s/%s/owned=%t/%s", provider, mode, owned, state), func(t *testing.T) {
						previous := acceptedGapStepsForTest(t)
						if state == "missing" {
							previous[1].AcceptedGapsJSON = nil
						} else {
							raw := strings.ReplaceAll(*previous[1].AcceptedGapsJSON, acceptedGapTestReason, "previous owner approval")
							previous[1].AcceptedGapsJSON = &raw
						}
						md, _ := buildPipelineSummaryFor(previous, nil, testPipelineHeadSHA, scm.ProviderGitHub, pipelineAttestationPolicy{})
						body := "### Accepted gaps\n\nAuthor discussion of unrelated gaps.\n\n" + appendixEvidence(mode, prBodyHTML, "low risk", "", md, provider) + "\n\nReviewer paragraph after the audit.\n"
						if owned {
							body = "Author prefix\n\n" + wrapPRAppendix(body) + "\nAuthor suffix"
						}
						current := acceptedGapStepsForTest(t)
						wantReason := acceptedGapTestReason
						if state == "removed" {
							current[1].AcceptedGapsJSON = nil
							wantReason = ""
						} else if state == "repair" {
							current = nil
							wantReason = "previous owner approval"
						}
						host := &attestationTestHost{provider: provider, body: body}
						head := strings.Repeat("ab", 20)
						if err := restampPRAttestationWithSteps(context.Background(), host, &scm.PR{Number: "42"}, head, current, nil, pipelineAttestationPolicy{}, nil); err != nil {
							t.Fatal(err)
						}
						got := host.body
						for _, surrounding := range []string{"### Accepted gaps\n\nAuthor discussion of unrelated gaps.", "\n\nReviewer paragraph after the audit.\n"} {
							if !strings.Contains(got, surrounding) {
								t.Fatalf("restamp lost surrounding content %q: %s", surrounding, got)
							}
						}
						attestation := parsePipelineAttestationForTest(t, got)
						if attestation.HeadSHA != head {
							t.Fatal("head not rebound")
						}
						var gap *pipelineAttestationAcceptedGaps
						for _, step := range attestation.Steps {
							if step.Step == types.StepTest {
								gap = step.AcceptedGaps
							}
						}
						if wantReason == "" {
							if gap != nil || strings.Contains(got, acceptedGapsStart) || strings.Contains(got, "previous owner approval") {
								t.Fatal("obsolete gap retained")
							}
						} else {
							if strings.Count(got, "Approved: "+wantReason) != 1 || strings.Count(got, "Covered by: "+acceptedGapTestCoveredBy) != 1 || gap == nil || gap.ReasonDigest != fmt.Sprintf("%x", sha256.Sum256([]byte(wantReason))) {
								t.Fatalf("audit and attestation disagree: %s", got)
							}
							if state != "repair" && strings.Contains(got, "previous owner approval") {
								t.Fatal("stale approval retained")
							}
						}
						if owned {
							parts, err := parsePROwnedBody(got)
							if err != nil || parts.before != "Author prefix\n\n" || parts.after != "\nAuthor suffix" {
								t.Fatalf("ownership changed: %+v %v", parts, err)
							}
						}
					})
				}
			}
		}
	}
}

func TestRestampAcceptedGapAuditRefusesOrdinaryOverflow(t *testing.T) {
	steps := acceptedGapStepsForTest(t)
	old := acceptedGapStepsForTest(t)
	old[1].AcceptedGapsJSON = nil
	md, _ := buildPipelineSummaryFor(old, nil, testPipelineHeadSHA, scm.ProviderAzureDevOps, pipelineAttestationPolicy{})
	body := joinBlocks(strings.Repeat("x", 3900-scm.PRBodyLen(md)), md)
	host := &attestationTestHost{provider: scm.ProviderAzureDevOps, body: body}
	if err := restampPRAttestationWithSteps(context.Background(), host, &scm.PR{Number: "42"}, strings.Repeat("ab", 20), steps, nil, pipelineAttestationPolicy{}, nil); err == nil {
		t.Fatal("impossible audit budget accepted")
	}
	if host.updates != 0 || host.body != body {
		t.Fatal("overflow reached publication")
	}
}

func TestAcceptedGapMarkersInOperatorTextAreNeutralized(t *testing.T) {
	for _, provider := range []scm.Provider{scm.ProviderGitHub, scm.ProviderBitbucket} {
		steps := acceptedGapStepsForTest(t)
		var gap db.AcceptedGaps
		if err := json.Unmarshal([]byte(*steps[1].AcceptedGapsJSON), &gap); err != nil {
			t.Fatal(err)
		}
		gap.Reason = "approved " + acceptedGapsStart
		gap.CoveredBy = "tests " + acceptedGapsEnd
		raw, err := json.Marshal(gap)
		if err != nil {
			t.Fatal(err)
		}
		encoded := string(raw)
		steps[1].AcceptedGapsJSON = &encoded
		block := buildAcceptedGapsBlock(steps, provider)
		if strings.Count(block, acceptedGapsStart) != 1 || strings.Count(block, acceptedGapsEnd) != 1 {
			t.Fatalf("operator text forged audit boundaries: %s", block)
		}
		text := neutralizeAttestationMarkers(provider, acceptedGapsStart+acceptedGapsEnd)
		if strings.Contains(text, acceptedGapsStart) || strings.Contains(text, acceptedGapsEnd) {
			t.Fatalf("narrative markers survived: %s", text)
		}
	}
}

func TestRestampAcceptedGapAuditRefusesAmbiguousMarkers(t *testing.T) {
	for _, markers := range []string{acceptedGapsStart, acceptedGapsEnd, acceptedGapsEnd + acceptedGapsStart, acceptedGapsStart + acceptedGapsStart + acceptedGapsEnd} {
		old := acceptedGapStepsForTest(t)
		old[1].AcceptedGapsJSON = nil
		md, _ := buildPipelineSummaryFor(old, nil, testPipelineHeadSHA, scm.ProviderGitHub, pipelineAttestationPolicy{})
		body := md + "\n\n" + markers
		host := &attestationTestHost{provider: scm.ProviderGitHub, body: body}
		if err := restampPRAttestationWithSteps(context.Background(), host, &scm.PR{Number: "42"}, strings.Repeat("ab", 20), acceptedGapStepsForTest(t), nil, pipelineAttestationPolicy{}, nil); err == nil {
			t.Fatal("ambiguous audit published")
		}
		if host.updates != 0 || host.body != body {
			t.Fatal("ambiguous audit changed live content")
		}
	}
}

func TestRestampedAcceptedGapDescriptionIsRedacted(t *testing.T) {
	t.Parallel()
	const description = "live validation verdict: inconclusive (1 of 2 scenarios were driven live against the product); untested: replay /Users/alice/projects/broker/scenario.json"
	steps := acceptedGapStepsForTest(t)
	gaps := `{"finding_ids":["test-verdict"],"findings":[{"id":"test-verdict","category":"test-verdict","description":"` + description + `"}],"reason":"` + acceptedGapTestReason + `","covered_by":"` + acceptedGapTestCoveredBy + `","verdict":"inconclusive"}`
	steps[1].AcceptedGapsJSON = &gaps

	// A body published before the gap was accepted, restamped through UpdatePR.
	plain := acceptedGapStepsForTest(t)
	plain[1].AcceptedGapsJSON = nil
	original, _ := buildPipelineSummaryFor(plain, nil, testPipelineHeadSHA, scm.ProviderGitHub, pipelineAttestationPolicy{})
	const newHead = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	restamped, ok, err := rebindOwnedPRAttestation(original, newHead, steps, pipelineAttestationPolicy{}, scm.ProviderGitHub)
	if err != nil || !ok {
		t.Fatalf("restamp = %v, %v", ok, err)
	}
	if !strings.Contains(restamped, acceptedGapsHeading) {
		t.Fatalf("restamp omitted the accepted-gap block:\n%s", restamped)
	}
	if strings.Contains(restamped, "/Users/alice") {
		t.Fatalf("restamped body leaks a home path:\n%s", restamped)
	}
	if !strings.Contains(restamped, "untested: replay ~") {
		t.Fatalf("restamped description was not redacted to the placeholder:\n%s", restamped)
	}

	owned := "author text\n\n" + wrapPRAppendix(original) + "\n\nsuffix"
	ownedRestamp, ok, err := rebindOwnedPRAttestation(owned, newHead, steps, pipelineAttestationPolicy{}, scm.ProviderGitHub)
	if err != nil || !ok {
		t.Fatalf("owned restamp = %v, %v", ok, err)
	}
	if strings.Contains(ownedRestamp, "/Users/alice") || !strings.Contains(ownedRestamp, "untested: replay ~") || !strings.HasPrefix(ownedRestamp, "author text") || !strings.HasSuffix(ownedRestamp, "suffix") {
		t.Fatalf("owned restamp not redacted or touched author text:\n%s", ownedRestamp)
	}
}

func TestBitbucketStyleRestampAddsReplacesAndRemovesTheAcceptedGapAudit(t *testing.T) {
	t.Parallel()
	withGap := acceptedGapStepsForTest(t)
	plain := acceptedGapStepsForTest(t)
	plain[1].AcceptedGapsJSON = nil
	original, _ := buildPipelineSummaryFor(plain, nil, testPipelineHeadSHA, scm.ProviderBitbucket, pipelineAttestationPolicy{})
	if strings.Contains(original, pipelineAttestationCommentPrefix) || strings.Contains(original, acceptedGapsStart) {
		t.Fatalf("fixture is not a Bitbucket-style body without attestation or audit:\n%s", original)
	}
	const newHead = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

	added, rebound, err := rebindOwnedPRAttestation(original, newHead, withGap, pipelineAttestationPolicy{}, scm.ProviderBitbucket)
	if err != nil || !rebound {
		t.Fatalf("add = %v, %v", rebound, err)
	}
	if !strings.Contains(added, acceptedGapsStart) || !strings.Contains(added, "Approved: "+acceptedGapTestReason) || strings.Contains(added, pipelineAttestationCommentPrefix) {
		t.Fatalf("audit not added to the attestation-free body:\n%s", added)
	}

	changed := acceptedGapStepsForTest(t)
	gaps := `{"finding_ids":["test-verdict"],"findings":[{"id":"test-verdict","category":"test-verdict","description":"inconclusive"}],"reason":"owner re-approved via first mate","covered_by":"TestOther","verdict":"inconclusive"}`
	changed[1].AcceptedGapsJSON = &gaps
	replaced, rebound, err := rebindOwnedPRAttestation(added, newHead, changed, pipelineAttestationPolicy{}, scm.ProviderBitbucket)
	if err != nil || !rebound || strings.Count(replaced, acceptedGapsStart) != 1 || !strings.Contains(replaced, "owner re-approved via first mate") || strings.Contains(replaced, acceptedGapTestReason) {
		t.Fatalf("audit not replaced: %v %v\n%s", rebound, err, replaced)
	}

	removed, rebound, err := rebindOwnedPRAttestation(replaced, newHead, plain, pipelineAttestationPolicy{}, scm.ProviderBitbucket)
	if err != nil || !rebound || strings.Contains(removed, acceptedGapsStart) || strings.Contains(removed, "owner re-approved") {
		t.Fatalf("audit not removed: %v %v\n%s", rebound, err, removed)
	}

	// A body that is not ours is never given an audit block.
	foreign, rebound, err := rebindOwnedPRAttestation("someone else's description", newHead, withGap, pipelineAttestationPolicy{}, scm.ProviderBitbucket)
	if err != nil || rebound || foreign != "someone else's description" {
		t.Fatalf("foreign body touched: %q %v %v", foreign, rebound, err)
	}
}
