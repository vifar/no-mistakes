package steps

import (
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/mdfence"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
)

// fenceLines renders n numbered evidence lines.
func fenceLines(n int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		b.WriteString("captured output line with some padding text\n")
	}
	return strings.TrimSuffix(b.String(), "\n")
}

func TestTruncateTextAtLineBoundaryClosesOpenFence(t *testing.T) {
	t.Parallel()
	marker := essentialPRBodyTruncationMarker()
	for _, tc := range []struct {
		name   string
		open   string
		closer string
	}{
		{"backtick", "```text", "```"},
		{"tilde", "~~~", "~~~"},
		{"long backtick run", "````markdown", "````"},
		{"long tilde run", "~~~~~", "~~~~~"},
	} {
		text := "## Testing\n\n" + tc.open + "\n" + fenceLines(200) + "\n" + tc.closer
		for _, budget := range []int{300, 301, 302, 303, 320, 500, 1000} {
			got := truncateTextAtLineBoundary(text, budget, marker)
			if len(got) > budget {
				t.Fatalf("%s budget %d: len = %d, want <= budget", tc.name, budget, len(got))
			}
			if mdfence.Scan(got).Open() {
				t.Fatalf("%s budget %d: truncated text ends inside an open fence:\n%s", tc.name, budget, got)
			}
			if !strings.Contains(got, "\n"+tc.closer+"\n\n"+marker) {
				t.Fatalf("%s budget %d: want closing fence %q before marker, got:\n%s", tc.name, budget, tc.closer, got)
			}
		}
	}
}

func TestTruncateTextAtLineBoundaryNestedBackticksInsideFence(t *testing.T) {
	t.Parallel()
	// A shorter backtick run, an info-string "fence" and a tilde run inside a
	// four-backtick fence are content, not fence boundaries.
	text := "````md\n```go\ncode\n```\n~~~\nnot a boundary\n" + fenceLines(100) + "\n````"
	got := truncateTextAtLineBoundary(text, 400, essentialPRBodyTruncationMarker())
	if len(got) > 400 || mdfence.Scan(got).Open() {
		t.Fatalf("len=%d open=%t:\n%s", len(got), mdfence.Scan(got).Open(), got)
	}
	if !strings.Contains(got, "\n````\n\n_... (body truncated") {
		t.Fatalf("want the four-backtick closer, got:\n%s", got)
	}
}

func TestTruncateTextAtLineBoundaryLeavesBalancedFencesAlone(t *testing.T) {
	t.Parallel()
	text := "```text\nfirst\n```\n\n" + fenceLines(100)
	got := truncateTextAtLineBoundary(text, 300, essentialPRBodyTruncationMarker())
	if strings.Count(got, "```") != 2 {
		t.Fatalf("balanced fence should not gain a closer:\n%s", got)
	}
}

func TestTruncatePipelineUpdateClosesOpenFence(t *testing.T) {
	t.Parallel()
	text := "```text\n" + fenceLines(100) + "\n```"
	got := truncatePipelineUpdateAtLineBoundary(text, 300, pipelineLatestUpdateTruncationMarker())
	if len(got) > 300 || mdfence.Scan(got).Open() {
		t.Fatalf("len=%d open=%t:\n%s", len(got), mdfence.Scan(got).Open(), got)
	}
}

func TestSplitPRBodySectionsIgnoresHeadingsInsideFences(t *testing.T) {
	t.Parallel()
	body := "## One\n\n```text\n## not a section\n```\n\n## Two\n\ntext"
	if got := splitPRBodySections(body); len(got) != 2 {
		t.Fatalf("sections = %q, want 2", got)
	}
}

func closingTestContext(refs ...string) *pipeline.StepContext {
	return &pipeline.StepContext{ClosingIssueRefs: refs}
}

// A large intent plus generated evidence whose newline cut lands inside the
// evidence fence must still publish a body whose Closes line is live.
func TestAssembleDraftPRBodyClosingReferenceSurvivesCutInsideEvidenceFence(t *testing.T) {
	t.Parallel()
	for _, limit := range []int{0, 4000, 20000} {
		sctx := closingTestContext("39")
		sctx.UserIntent = "Large published intent.\n" + strings.Repeat("intent context line that fills the budget\n", 2500)
		testingMD := "## Testing\n\nArtifact output:\n\n```text\n" + fenceLines(2500) + "\n```"
		pipelineMD := pipelineMarkdownForTest("review round 001")

		got := assembleDraftPRBody(sctx, "## What Changed\n\n- summary", "Low: test", testingMD, pipelineMD, limit, scm.ProviderGitHub)

		if limit == 0 {
			assertGitHubBodyLimitForTest(t, got)
		} else if scm.PRBodyLen(got) > limit {
			t.Fatalf("limit %d: body is %d units", limit, scm.PRBodyLen(got))
		}
		if mdfence.Scan(got).Open() {
			t.Fatalf("limit %d: body ends inside an open fence", limit)
		}
		if err := verifyClosingIssuesInBody(got, sctx); err != nil {
			t.Fatalf("limit %d: %v\ntail:\n%s", limit, err, got[max(len(got)-400, 0):])
		}
		if !strings.HasSuffix(got, "## Issues\n\nCloses #39") {
			t.Fatalf("limit %d: Issues section is not last, tail:\n%s", limit, got[max(len(got)-200, 0):])
		}
	}
}

// Generated sections that alone exceed the budget are cut mid-fence: this is
// the reported failure mode.
func TestAssembleDraftPRBodyClosingReferenceSurvivesOversizedGeneratedFence(t *testing.T) {
	t.Parallel()
	sctx := closingTestContext("39")
	sctx.UserIntent = strings.Repeat("intent context line that fills the budget\n", 1500)
	testingMD := "## Testing\n\n~~~text\n" + fenceLines(2500) + "\n~~~"

	got := assembleDraftPRBody(sctx, "", "", testingMD, "", 0, scm.ProviderGitHub)

	assertGitHubBodyLimitForTest(t, got)
	if err := verifyClosingIssuesInBody(got, sctx); err != nil {
		t.Fatalf("%v\ntail:\n%s", err, got[max(len(got)-400, 0):])
	}
}

// The assembly backstop closes a fence a builder left open.
func TestAppendIssuesSectionClosesOpenFence(t *testing.T) {
	t.Parallel()
	sctx := closingTestContext("39")
	section := issuesSection(sctx, "")
	got := appendIssuesSection("## Testing\n\n````text\nline", section)
	if err := verifyClosingIssuesInBody(got, sctx); err != nil {
		t.Fatalf("%v\n%s", err, got)
	}
	if !strings.Contains(got, "line\n````\n\n## Issues") {
		t.Fatalf("want the four-backtick closer before Issues, got:\n%s", got)
	}
	if got := appendIssuesSection("balanced\n\n```\nx\n```", section); strings.Count(got, "```") != 2 {
		t.Fatalf("balanced body gained a closer:\n%s", got)
	}
}

func TestFenceClosedWithinRebuildsWhenBuilderLeavesFenceOpen(t *testing.T) {
	t.Parallel()
	var budgets []int
	got := fenceClosedWithin(100, func(s string) int { return len(s) }, func(budget int) string {
		budgets = append(budgets, budget)
		return "```\nbody"
	})
	if len(budgets) != 2 || budgets[1] != 100-len("\n```") {
		t.Fatalf("builder budgets = %v, want a rebuild with room for the closing line", budgets)
	}
	if got == "" {
		t.Fatal("empty body")
	}
}

// The verifier must keep ignoring a closing line that sits inside a fence.
func TestVerifyClosingIssuesStillRejectsReferenceInsideFence(t *testing.T) {
	t.Parallel()
	for _, fence := range []string{"```", "~~~", "````"} {
		sctx := closingTestContext("39")
		body := "## Testing\n\n" + fence + "text\noutput\n\n## Issues\n\nCloses #39"
		if err := verifyClosingIssuesInBody(body, sctx); err == nil {
			t.Fatalf("%s: Closes #39 inside an open fence was accepted", fence)
		}
		closed := "## Testing\n\n" + fence + "text\nCloses #39\n" + fence
		if err := verifyClosingIssuesInBody(closed, sctx); err == nil {
			t.Fatalf("%s: Closes #39 inside a closed fence was accepted", fence)
		}
	}
	// Unrequested references stay non-closing.
	if err := verifyClosingIssuesInBody("## Issues\n\nCloses #40", closingTestContext("39")); err == nil {
		t.Fatal("unrequested reference was accepted")
	}
}

func TestVerifyClosingIssuesRejectionLogsSanitizedBody(t *testing.T) {
	t.Parallel()
	var logged []string
	sctx := closingTestContext("39")
	sctx.Log = func(s string) { logged = append(logged, s) }
	body := "## Intent\n\n" + strings.Repeat("filler line\n", 400) +
		"## Testing\n\n```text\nran /home/testuser/work/project/tool\n\n## Issues\n\nCloses #39\n" +
		strings.Repeat("trailing evidence\n", 300)

	if err := verifyClosingIssuesInBody(body, sctx); err == nil {
		t.Fatal("expected rejection")
	}
	if len(logged) != 1 {
		t.Fatalf("logged %d entries, want 1", len(logged))
	}
	entry := logged[0]
	for _, want := range []string{
		"Closes #39",
		"ended inside an open fence: true",
		"Issues section started inside an open fence: true",
		"## Issues",
		"[... ",
		"body is ",
	} {
		if !strings.Contains(entry, want) {
			t.Fatalf("log missing %q:\n%s", want, entry)
		}
	}
	if strings.Contains(entry, "/home/testuser") {
		t.Fatalf("log leaked a home path:\n%s", entry)
	}
	if len(entry) > closingLogHeadBytes+2*closingLogWindowBytes+closingLogTailBytes+1000 {
		t.Fatalf("log excerpt is %d bytes, want it capped", len(entry))
	}
}
