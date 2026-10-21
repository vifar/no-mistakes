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
		name          string
		merged        bool
		publish       bool
		advanceTarget bool
		changeMerge   bool
		retarget      bool
		foreignHead   bool
		divergentHead bool
		stalePublish  bool
		wantError     string
	}{
		{name: "merged with a pipeline-published repair", merged: true, publish: true},
		{name: "closed with a pipeline-published repair", publish: true},
		{name: "merged before another target advance", merged: true, publish: true, advanceTarget: true},
		{name: "merge changed the repair", merged: true, publish: true, changeMerge: true, wantError: "merged PR tree differs"},
		{name: "merged into a different branch", merged: true, publish: true, retarget: true, wantError: "matching PR receipt"},
		{name: "closed after retargeting", publish: true, retarget: true, wantError: "matching PR receipt"},
		{name: "closed after another publisher advances the forge head", publish: true, foreignHead: true, wantError: "matching PR receipt"},
		{name: "closed with a published head on a different lineage", publish: true, divergentHead: true, wantError: "matching PR receipt"},
		{name: "closed with only an older publication record", publish: true, stalePublish: true, wantError: "matching PR receipt"},
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
			if tc.divergentHead {
				gitCmd(t, dir, "reset", "--hard", base)
			}
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
				publishedHead := repairHead
				if tc.stalePublish {
					publishedHead = receiptHead
				}
				if err := sctx.DB.UpdateRunPublication(sctx.Run.ID, db.PushBinding{
					HeadSHA: publishedHead, TargetKind: "upstream", TargetFingerprint: "test", Ref: "refs/heads/feature",
				}); err != nil {
					t.Fatal(err)
				}
				if tc.stalePublish {
					if err := sctx.DB.UpdateRunHeadSHA(sctx.Run.ID, repairHead); err != nil {
						t.Fatal(err)
					}
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
				if tc.changeMerge {
					if err := os.WriteFile(filepath.Join(dir, "repair.txt"), []byte("broken again\n"), 0o644); err != nil {
						t.Fatal(err)
					}
					gitCmd(t, dir, "add", "repair.txt")
					gitCmd(t, dir, "commit", "--amend", "--no-edit")
				}
				mergedSHA = gitCmd(t, dir, "rev-parse", "HEAD")
				if tc.advanceTarget {
					gitCmd(t, dir, "commit", "--allow-empty", "-m", "later target commit")
				}
				gitCmd(t, dir, "checkout", "feature")
			}
			targetBranch := "main"
			if tc.retarget {
				targetBranch = "release"
				gitCmd(t, dir, "branch", targetBranch, "main")
			}
			forgeHead := repairHead
			if tc.foreignHead {
				// Another publisher can advance the forge while this run still
				// has its own published repair checked out. Local ancestry alone
				// must not authorize that different live head.
				gitCmd(t, dir, "commit", "--allow-empty", "-m", "another publisher")
				forgeHead = gitCmd(t, dir, "rev-parse", "HEAD")
				gitCmd(t, dir, "reset", "--hard", repairHead)
			}
			facts := fmt.Sprintf(`{"number":42,"html_url":%q,"state":"closed","merged":%t,"merge_commit_sha":%q,"head":{"ref":"feature","sha":%q,"repo":{"full_name":"test/repo"}},"base":{"ref":%q}}`, prURL, merged, mergedSHA, forgeHead, targetBranch)
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
			if tc.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantError) {
					t.Fatalf("expected %q, got decision %+v, error %v", tc.wantError, decision, err)
				}
				if tc.retarget || tc.foreignHead || tc.divergentHead || tc.stalePublish {
					unchanged, readErr := sctx.DB.GetRunPRContext(sctx.Run.ID)
					if readErr != nil {
						t.Fatal(readErr)
					}
					if unchanged.PRContextCandidate != receipt.PRContextCandidate {
						t.Fatalf("unproven advance replaced receipt: %+v", unchanged)
					}
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
			if advanced.TargetBranch != receipt.TargetBranch || advanced.TargetSHA != receipt.TargetSHA || advanced.MergeBaseSHA != receipt.MergeBaseSHA {
				t.Fatalf("advance lost the original target comparison: before %+v, after %+v", receipt, advanced)
			}
			if advanced.DiffDigest == receipt.DiffDigest {
				t.Fatal("advance kept the pre-repair diff digest")
			}
			if advanced.LocalHeadSHA != repairHead {
				t.Fatalf("receipt did not follow the pipeline's own advance: %+v", advanced)
			}
		})
	}
}
