//go:build e2e && !windows

package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Drive the real CLI, daemon and Git against the owned fake forge. Inject a
// single gh credential-store failure only after publication and target advance;
// the original comparison and completed Review/Test rows are the oracle.
func TestIssue100PublishedTargetTimeoutLiveJourney(t *testing.T) {
	for _, read := range []string{"protection/required_status_checks", "mergeable,mergeStateStatus"} {
		t.Run(read, func(t *testing.T) {
			j := newScopeJourney(t, SetupOpts{Agent: "claude", Scenario: cleanReviewScenario(t)})
			j.setTrusted("commands:\n  test: 'true'\n  lint: 'true'\n")
			j.setForge(forgeState{Protection: &forgeEndpoint{Body: []byte(`{"strict":false,"contexts":[],"checks":[]}`)}, Checks: []forgeCheck{greenCheck("build")}})
			j.init()
			const branch = "feature/issue-100-timeout"
			j.push(branch)
			run := j.expectReady(branch, 90*time.Second)
			database, err := db.OpenReadOnly(filepath.Join(j.h.NMHome, "state.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()
			receipt, err := database.GetRunPRContext(run.ID)
			if err != nil || receipt == nil || !receipt.CIQualified {
				t.Fatalf("missing qualified comparison: %+v %v", receipt, err)
			}
			before, err := database.GetStepsByRun(run.ID)
			if err != nil {
				t.Fatal(err)
			}
			head := j.h.UpstreamBranchSHA(branch)
			body := livePRBody(t, j.statePath, branch)

			// Keep the stub's gh basename so its normal dispatcher still works.
			stubDir := filepath.Join(j.h.BinDir, "original")
			if err := os.Mkdir(stubDir, 0700); err != nil {
				t.Fatal(err)
			}
			gh := filepath.Join(j.h.BinDir, "gh")
			stub := filepath.Join(stubDir, "gh")
			if err := os.Rename(gh, stub); err != nil {
				t.Fatal(err)
			}
			consumed := filepath.Join(j.h.NMHome, "keyring-timeout-consumed")
			armed := filepath.Join(j.h.NMHome, "retention-read-started")
			if strings.HasPrefix(read, "protection/") {
				if err := os.WriteFile(armed, nil, 0600); err != nil {
					t.Fatal(err)
				}
			}
			// A normal CI poll checks mergeability before verifying its PR
			// comparison. Arm that fault only once retention reads branch rules.
			script := fmt.Sprintf(`#!/bin/sh
for arg do
  case "$arg" in
    */rules/branches/*)
      target=$(git --git-dir=%s rev-parse refs/heads/main) || exit 2
      if [ "$target" != %s ]; then : > %s; fi
      ;;
  esac
  case "$arg" in
    *%s*)
      target=$(git --git-dir=%s rev-parse refs/heads/main) || exit 2
      if [ "$target" != %s ] && [ -f %s ] && mkdir %s 2>/dev/null; then
        printf 'Timeout trying to log in to github.com account test (keyring)\n' >&2
        exit 1
      fi
      ;;
  esac
done
exec %s "$@"
`, shellQuote(j.h.UpstreamDir), shellQuote(receipt.TargetSHA), shellQuote(armed), read, shellQuote(j.h.UpstreamDir), shellQuote(receipt.TargetSHA), shellQuote(armed), shellQuote(consumed), shellQuote(stub))
			if err := os.WriteFile(gh, []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			advanceMain(t, j.h, "advanced.txt", "advanced target\n", "advance target before transient read")
			target := j.h.UpstreamBranchSHA("main")
			deadline := pollDeadline(t, 90*time.Second)
			for time.Now().Before(deadline) {
				retained, err := database.GetRetainedPRTarget(run.ID, receipt)
				if err != nil {
					t.Fatal(err)
				}
				if retained == target {
					break
				}
				time.Sleep(200 * time.Millisecond)
			}
			retained, err := database.GetRetainedPRTarget(run.ID, receipt)
			if err != nil || retained != target || !strings.Contains(j.ciLog(run.ID), "retrying PR-context read: attempt 2 of 3") || !strings.Contains(j.ciLog(run.ID), "class=transient keyring timeout") {
				t.Fatalf("timeout did not recover within the same read: retained=%s want=%s error=%v\n%s", retained, target, err, j.ciLog(run.ID))
			}
			current, err := database.GetRunPRContext(run.ID)
			if err != nil || !reflect.DeepEqual(current, receipt) || j.h.UpstreamBranchSHA(branch) != head || livePRBody(t, j.statePath, branch) != body {
				t.Fatalf("retry changed qualification, published head or attestation: %+v -> %+v %v", receipt, current, err)
			}
			after, err := database.GetStepsByRun(run.ID)
			if err != nil {
				t.Fatal(err)
			}
			for i, step := range before {
				if (step.StepName == types.StepReview || step.StepName == types.StepTest) && !reflect.DeepEqual(step, after[i]) {
					t.Fatalf("retry replayed completed validation: %+v -> %+v", step, after[i])
				}
			}
			if info := j.h.RunInfo(run.ID); !info.CIReady || info.Status != types.RunRunning {
				t.Fatalf("retry lost the active qualified run: %+v", info)
			}
			t.Logf("same run %s recovered %s, preserving Review, Test, comparison and head %s\n%s", run.ID, read, head, j.ciLog(run.ID))
			j.finish(branch)
		})
	}
}
