//go:build e2e

package e2e

import (
	"strings"
	"testing"
)

// Standalone closing declarations request closure; ordinary prose and URL
// references stay neutralized. The public PR-body contract is documented in
// reference/pipeline-steps.md. This rejects both dropping requested closures
// and accidentally promoting a passing reference in prose.
func TestClosingIssueRefsPublishedIntentPromotesOnlyStandaloneDeclarations(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude"})
	statePath := setupStatefulGitHub(t, h)
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	const branch = "feature/closes-neutralize"
	h.CommitChange(branch, "n.txt", "n\n", "add neutralize fixture")
	wt := h.AddWorktree(branch)
	intent := "Refactor the widget; this fixes #16 partly.\nFixes #12\nResolves: acme/widgets#3\nCloses https://github.com/example/closes/issues/14\nfixed http://github.com/example/closes/pull/15"
	if out, err := h.RunInDir(wt, "axi", "run", "--intent", intent, "--skip", "ci"); err != nil {
		t.Fatalf("axi run: %v\n%s", err, out)
	}
	waitPublished(t, h, branch, "", "promote standalone declarations")
	body := livePRBody(t, statePath, branch)
	saveEvidence(t, "14-promoted-intent-pr-body.md", body)
	assertLinesOnce(t, "standalone declarations", body, "## Issues", "Closes #12", "Closes acme/widgets#3")
	if !strings.Contains(body, "## Issues\n\nCloses #12\nCloses acme/widgets#3") {
		t.Errorf("standalone declarations must render as plain Issues lines; body:\n%s", body)
	}
	for _, want := range []string{
		"Refactor the widget; this fixes `#16` partly.",
		"Closes `https://github.com/example/closes/issues/14`",
		"fixed `http://github.com/example/closes/pull/15`",
	} {
		if exactLineCount(body, want) != 1 {
			t.Errorf("neutralized line %q not published exactly once; body:\n%s", want, body)
		}
	}
	for _, live := range []string{
		"Fixes #12",
		"Resolves: acme/widgets#3",
		"Fixes `#12`",
		"Resolves: `acme/widgets#3`",
		"Closes #16",
		"Closes https://github.com/example/closes/issues/14",
		"fixed http://github.com/example/closes/pull/15",
	} {
		if strings.Contains(body, live) {
			t.Errorf("unrequested or repeated closing text %q published; body:\n%s", live, body)
		}
	}
}
