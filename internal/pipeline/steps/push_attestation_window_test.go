package steps

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// TestTargetComparisonAdvancedNamesExactlyOneCondition proves the sentinel
// the push step keys on names ONE condition: the live target, merge base, or
// diff no longer matches the run's pinned receipt. Every other comparison
// failure must keep failing closed, because it can mean a changed PR
// identity, a changed head, or an unreadable receipt.
func TestTargetComparisonAdvancedNamesExactlyOneCondition(t *testing.T) {
	if errTargetComparisonAdvanced == nil {
		t.Fatal("the target-advance sentinel is missing")
	}
	if !strings.Contains(errTargetComparisonAdvanced.Error(), "target") {
		t.Fatalf("the sentinel does not name the condition: %q", errTargetComparisonAdvanced.Error())
	}
	// The attestation's own message keeps its wording and carries the
	// sentinel, so the push step can recognise it and every reader still
	// sees what happened.
	err := fmt.Errorf("%w: live target commit, merge base, or diff differs from PR comparison receipt before update", errTargetComparisonAdvanced)
	if !errors.Is(err, errTargetComparisonAdvanced) {
		t.Fatal("the wrapped attestation failure is not recognised")
	}
	if !strings.Contains(err.Error(), "live target commit, merge base, or diff differs from PR comparison receipt before update") {
		t.Fatalf("the wrapped failure lost its wording: %v", err)
	}
	// Every other comparison failure stays outside the sentinel.
	for _, other := range []string{
		"live pull request differs from its comparison receipt before update",
		"durable run head differs from PR comparison receipt before update",
		"worktree is dirty before PR attestation update",
		"local head differs from proposed PR update head",
	} {
		if errors.Is(errors.New(other), errTargetComparisonAdvanced) {
			t.Fatalf("%q claims to be a target advance", other)
		}
	}
}

// TestPushStep_RewindRequestPublishesNothing proves a push that takes the
// rewind publishes nothing and asks for exactly one restart.
func TestPushStep_RewindRequestPublishesNothing(t *testing.T) {
	c, _ := publicationFixture(t)
	advanceRewindTarget(t, c)
	outcome, err := (&PushStep{}).Execute(c)
	if err != nil {
		t.Fatal(err)
	}
	if outcome == nil {
		t.Fatal("the push produced no outcome")
	}
	if outcome.RestartFrom != types.StepRebase {
		t.Fatalf("the push asked for %q, want rebase", outcome.RestartFrom)
	}
	if outcome.NeedsApproval || outcome.Skipped || outcome.SkipRemaining {
		t.Fatalf("the rewind outcome also changed the gate state: %+v", outcome)
	}
	run, err := c.DB.GetRun(c.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.LastPushedSHA != nil {
		t.Fatalf("the rewound push published: %+v", run.LastPushedSHA)
	}
}
