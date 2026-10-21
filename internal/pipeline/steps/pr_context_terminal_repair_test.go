package steps

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// TestPRContextGuardAcceptsTerminalCIRepairAdvanceOnAMergedPR covers the
// trigger measured on run 01M55FMH5ETDEFP7R2M7HAXZ68 (PR #61):
//
//  1. the CI step auto-fixes and the run PUBLISHES the new head,
//  2. the PR reaches merged/closed,
//  3. the CI monitor observes the terminal PR state,
//  4. the post-CI guard runs with PRContextAfterStep.
//
// The receipt still named the pre-repair head, so the terminal guard failed
// the run with "terminal CI state lacks a matching PR receipt" - even though
// the PR was merged and the repair was in main. The pipeline's own
// publication record proves the advance, so the receipt must follow it.
func TestPRContextGuardAcceptsTerminalCIRepairAdvanceOnAMergedPR(t *testing.T) {
	for _, tc := range []struct {
		name    string
		merged  bool
		publish bool
	}{
		{name: "merged with a pipeline-published repair", merged: true, publish: true},
		{name: "closed with a pipeline-published repair", publish: true},
		// The negative: the SAME head advance, but the run never published it
		// (no durable publication record), so the proof is absent and the
		// guard must still fail closed.
		{name: "merged with an unpublished repair", merged: true, publish: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, base, head := setupGitRepo(t)
			sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, base, head, config.Commands{})
			prURL := "https://github.com/test/repo/pull/42"
			selection := pipeline.PRTargetSelection{PRURL: prURL, SourceRepo: "test/repo", SourceBranch: "feature", ForgeHeadSHA: head, TargetBranch: "main"}
			if _, err := guardPRContextWithSelection(sctx, types.StepPR, selection, false); err != nil {
				t.Fatal(err)
			}
			receipt, err := sctx.DB.GetRunPRContext(sctx.Run.ID)
			if err != nil {
				t.Fatal(err)
			}
			receiptHead := receipt.LocalHeadSHA

			// The CI step commits a repair, which advances the head.
			if err := os.WriteFile(filepath.Join(dir, "repair.txt"), []byte("fixed\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			gitCmd(t, dir, "add", "repair.txt")
			gitCmd(t, dir, "commit", "-m", "ci repair")
			repairHead := gitCmd(t, dir, "rev-parse", "HEAD")
			if repairHead == receiptHead {
				t.Fatal("the repair did not advance the head")
			}
			// The run publishes the repair head: the durable publication record
			// is the proof that the advance was the pipeline's own.
			if tc.publish {
				if err := sctx.DB.UpdateRunPublication(sctx.Run.ID, db.PushBinding{
					HeadSHA: repairHead, TargetKind: "upstream", TargetFingerprint: "test", Ref: "refs/heads/feature",
				}); err != nil {
					t.Fatal(err)
				}
			}
			sctx.Run.HeadSHA = repairHead

			// The PR reaches a terminal state, and the monitor observes it.
			state := "closed"
			if tc.merged {
				state = "merged"
			}
			if err := sctx.DB.UpdateRunPRState(sctx.Run.ID, state); err != nil {
				t.Fatal(err)
			}
			sctx.Run.PRState = &state
			merged := tc.merged
			mergedSHA := ""
			if merged {
				// The merge result is a real merge of the target and the
				// published repair head, so the terminal comparison can
				// prove the merge tree against the reviewed comparison.
				gitCmd(t, dir, "checkout", "main")
				gitCmd(t, dir, "merge", "--no-ff", "feature", "-m", "merge published repair")
				mergedSHA = gitCmd(t, dir, "rev-parse", "HEAD")
				gitCmd(t, dir, "checkout", "feature")
			}
			facts := fmt.Sprintf(`{"number":42,"html_url":%q,"state":"closed","merged":%t,"merge_commit_sha":%q,"head":{"ref":"feature","sha":%q,"repo":{"full_name":"test/repo"}},"base":{"ref":"main"}}`, prURL, merged, mergedSHA, repairHead)
			binDir := fakeCLIBinDir(t)
			linkTestBinary(t, binDir, "gh")
			sctx.Env = fakeCLIEnv(binDir, map[string]string{
				"FAKE_CLI_MODE":            "ci-gh-reconcile",
				"FAKE_CLI_PR_FACTS_JSON":   facts,
				"FAKE_CLI_PROTECTION_JSON": `{"strict":false}`,
				"FAKE_CLI_RULES_JSON":      `[]`,
			})
			sctx.PRContextAfterStep = true

			decision, err := GuardPRContext(sctx, types.StepCI)
			if !tc.publish {
				// Without the publication record the advance is unproven, so
				// the guard must still fail closed.
				if err == nil {
					t.Fatalf("accepted an unproven head advance: %+v", decision)
				}
				if !strings.Contains(err.Error(), "matching PR receipt") {
					t.Fatalf("unexpected refusal: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("terminal guard refused a pipeline-published repair: %v", err)
			}
			if decision.Target.PRURL != prURL {
				t.Fatalf("terminal selection = %+v", decision)
			}
			// The receipt must follow the pipeline's own advance.
			advanced, err := sctx.DB.GetRunPRContext(sctx.Run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if advanced.LocalHeadSHA != repairHead {
				t.Fatalf("receipt did not follow the pipeline's own advance: %+v", advanced)
			}
		})
	}
}
