package steps

import (
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func retainedTargetFixture(t *testing.T) (*pipeline.StepContext, pipeline.PRTargetSelection, *db.PRContext) {
	t.Helper()
	dir, base, head := setupGitRepo(t)
	c := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, base, head, config.Commands{})
	selection := pipeline.PRTargetSelection{PRURL: "https://github.com/test/repo/pull/42", SourceRepo: "test/repo", SourceBranch: "feature", ForgeHeadSHA: head, TargetBranch: "main"}
	if _, err := guardPRContextWithSelection(c, types.StepPR, selection, false); err != nil {
		t.Fatal(err)
	}
	for _, err := range []error{c.DB.UpdateRunReviewApprovedHeadSHA(c.Run.ID, head), c.DB.UpdateRunPushBinding(c.Run.ID, db.PushBinding{HeadSHA: head}), c.DB.SetRunCIReady(c.Run.ID, true)} {
		if err != nil {
			t.Fatal(err)
		}
	}
	bin := fakeCLIBinDir(t)
	linkTestBinary(t, bin, "gh")
	c.Env = fakeCLIEnv(bin, map[string]string{"FAKE_CLI_MODE": "ci-gh", "FAKE_CLI_PROTECTION_JSON": `{"strict":false}`, "FAKE_CLI_RULES_JSON": `[]`, "FAKE_CLI_MERGEABLE": "MERGEABLE"})
	receipt, err := c.DB.GetRunPRContext(c.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	return c, selection, receipt
}

func TestRetainedTargetObservationContinuity(t *testing.T) {
	for _, path := range []string{"guard", "verify", "retain"} {
		for _, kind := range []string{"sibling", "rollback", "consecutive", "same"} {
			t.Run(path+"/"+kind, func(t *testing.T) {
				c, selection, receipt := retainedTargetFixture(t)
				gitCmd(t, c.WorkDir, "checkout", "main")
				gitCmd(t, c.WorkDir, "commit", "--allow-empty", "-m", "T1")
				first := gitCmd(t, c.WorkDir, "rev-parse", "HEAD")
				gitCmd(t, c.WorkDir, "checkout", "feature")
				if _, err := guardPRContextWithSelection(c, types.StepCI, selection, false); err != nil {
					t.Fatal(err)
				}
				gitCmd(t, c.WorkDir, "checkout", "main")
				switch kind {
				case "sibling", "rollback":
					gitCmd(t, c.WorkDir, "reset", "--hard", receipt.TargetSHA)
				}
				if kind == "sibling" || kind == "consecutive" {
					gitCmd(t, c.WorkDir, "commit", "--allow-empty", "-m", "T2")
				}
				target := gitCmd(t, c.WorkDir, "rev-parse", "HEAD")
				wantError := kind == "sibling" || kind == "rollback"
				_, oracleErr := git.Run(c.Ctx, c.WorkDir, "merge-base", "--is-ancestor", first, target)
				if (oracleErr != nil) != wantError {
					t.Fatalf("invalid Git ancestry fixture: %v", oracleErr)
				}
				gitCmd(t, c.WorkDir, "checkout", "feature")
				host, _ := buildHost(c, resolvedProvider(c))
				var err error
				switch path {
				case "guard":
					_, err = guardPRContextWithSelection(c, types.StepCI, selection, false)
				case "verify":
					err = verifyCurrentPRComparison(c, host, selection, receipt, nil)
				case "retain":
					candidate, e := readPRComparison(c, selection)
					if e != nil {
						t.Fatal(e)
					}
					candidate.PRURL, candidate.SourceRepo, candidate.SourceBranch, candidate.ForgeHeadSHA = receipt.PRURL, receipt.SourceRepo, receipt.SourceBranch, receipt.ForgeHeadSHA
					var keep bool
					keep, err = retainPublishedComparison(c, host, receipt, candidate)
					if !wantError && !keep {
						t.Fatalf("valid observation not retained: %v", err)
					}
				}
				if (err != nil) != wantError {
					t.Fatalf("observation error=%v; want refusal=%t", err, wantError)
				}
				expectedTarget := target
				if wantError {
					expectedTarget = first
				}
				if got, e := c.DB.GetRetainedPRTarget(c.Run.ID, receipt); e != nil || got != expectedTarget {
					t.Fatalf("retained=%q want=%q error=%v", got, expectedTarget, e)
				}
				if after, e := c.DB.GetRunPRContext(c.Run.ID); e != nil || *after != *receipt {
					t.Fatalf("receipt changed: %+v %v", after, e)
				}
			})
		}
	}
}
