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

func TestAppendIssuesSectionAfterOpenFenceFitsBudget(t *testing.T) {
	t.Parallel()
	const limit = 100
	sctx := closingTestContext("39")
	section := issuesSection(sctx, "")
	body := fenceClosedWithin(limit-len("\n\n"+section), func(s string) int { return len(s) }, func(budget int) string {
		return "```text\n" + strings.Repeat("x", budget-len("```text\n"))
	})
	got := appendIssuesSection(body, section)
	if len(got) > limit {
		t.Fatalf("assembled body is %d bytes, want <= %d:\n%s", len(got), limit, got)
	}
	if err := verifyClosingIssuesInBody(got, sctx); err != nil {
		t.Fatalf("%v\n%s", err, got)
	}
}

func TestAppendIssuesSectionAfterNBSPFenceSuffix(t *testing.T) {
	t.Parallel()
	sctx := closingTestContext("39")
	body := "```text\noutput\n```\u00a0"
	section := issuesSection(sctx, "")
	if err := verifyClosingIssuesInBody(body+"\n\n"+section, sctx); err == nil {
		t.Fatal("NBSP-suffixed fence must leave the closing reference inside code")
	}
	got := appendIssuesSection(body, section)
	if want := body + "\n```\n\n" + section; got != want {
		t.Fatalf("assembled body = %q, want %q", got, want)
	}
	if err := verifyClosingIssuesInBody(got, sctx); err != nil {
		t.Fatalf("%v\n%s", err, got)
	}
}

func TestPRFenceLineEndings(t *testing.T) {
	t.Parallel()
	for _, ending := range []string{"\n", "\r\n", "\r"} {
		for _, suffix := range []string{"", "\t", "\u00a0"} {
			sctx := closingTestContext("39")
			body := strings.Join([]string{"```text", "Closes #40", "```" + suffix, "postscript"}, ending)
			section := issuesSection(sctx, "")
			want := body
			if suffix == "\u00a0" {
				want += "\n```"
			}
			want += "\n\n" + section
			got := appendIssuesSection(body, section)
			if got != want {
				t.Fatalf("ending %q suffix %q: assembled = %q, want %q", ending, suffix, got, want)
			}
			if err := verifyClosingIssuesInBody(got, sctx); err != nil {
				t.Fatalf("ending %q suffix %q: %v", ending, suffix, err)
			}
			if err := verifyClosingIssuesInBody(body+ending+"Closes #41", closingTestContext("41")); (err != nil) != (suffix == "\u00a0") {
				t.Fatalf("ending %q suffix %q: unexpected verification result: %v", ending, suffix, err)
			}
			neutralized := neutralizeGitHubClosingReferences(body + ending + "Closes #41")
			wantNeutralized := "```text\nCloses #40\n```" + suffix + "\npostscript\nCloses "
			if suffix == "\u00a0" {
				wantNeutralized += "#41"
			} else {
				wantNeutralized += "`#41`"
			}
			if neutralized != wantNeutralized {
				t.Fatalf("ending %q suffix %q: neutralized = %q, want %q", ending, suffix, neutralized, wantNeutralized)
			}
		}
		body := strings.Join([]string{"## One", "```text", "## Hidden", "```", "## Two", "text"}, ending)
		if sections := splitPRBodySections(body); len(sections) != 2 || sections[1] != "## Two\ntext" {
			t.Fatalf("ending %q: sections = %q", ending, sections)
		}
		template := strings.ReplaceAll(body, "## ", "# ")
		if got := strings.Join(templateStructureLines(template), "\n"); got != "# One\n# Two" {
			t.Fatalf("ending %q: template headings = %q", ending, got)
		}
	}
}

func TestPRFenceLineEndingsTruncation(t *testing.T) {
	t.Parallel()
	for _, ending := range []string{"\n", "\r\n", "\r"} {
		body := strings.Join([]string{"```text", "output", "```", strings.Repeat("prose"+ending, 200)}, ending)
		for name, truncate := range map[string]func(string) string{
			"body":   func(s string) string { return truncateTextAtLineBoundary(s, 300, "truncated") },
			"update": func(s string) string { return truncatePipelineUpdateAtLineBoundary(s, 300, "truncated") },
			"clamp":  func(s string) string { return scm.ClampPRBody(s, 300) },
		} {
			got := truncate(body)
			if scm.PRBodyLen(got) > 300 || (name != "clamp" && len(got) > 300) || strings.Count(got, "```") != 2 {
				t.Fatalf("%s ending %q: truncation changed a balanced fence or exceeded budget: %q", name, ending, got)
			}
			sctx := closingTestContext("39")
			if err := verifyClosingIssuesInBody(appendIssuesSection(got, issuesSection(sctx, "")), sctx); err != nil {
				t.Fatalf("%s ending %q: %v", name, ending, err)
			}
		}
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
