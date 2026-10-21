package steps

import (
	"bytes"
	"context"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/git"
)

func TestAttributionExplicitTargetsOnly(t *testing.T) {
	f := newMergeFixture(t, false)
	gitCmd(t, f.dir, "checkout", "-b", "unrelated", f.baseSHA)
	gitCmd(t, f.dir, "commit", "--allow-empty", "-m", "unrelated history\n\nCodex-Session: unrelated")
	unrelated := gitCmd(t, f.dir, "rev-parse", "HEAD")
	original := storedReplayMessage(t, f.dir, unrelated)
	gitCmd(t, f.dir, "update-ref", "refs/remotes/origin/unrelated", unrelated)
	gitCmd(t, f.dir, "checkout", "feature")
	c := f.context(t, &mockAgent{}, "rebase")
	run := func(args ...string) (string, error) { return git.Run(context.Background(), f.dir, args...) }
	for _, targets := range [][]string{nil, {f.mainSHA}, nil} {
		if _, err := c.CommitAttributionEnv(nil, run, targets...); err != nil {
			t.Fatal(err)
		}
		if err := c.ValidateCommitAttribution(f.headSHA, unrelated, run); err == nil {
			t.Error("unrelated remote history became exempt")
		}
	}
	gitCmd(t, f.dir, "reset", "--hard", unrelated)
	if _, err := updateHeadSHA(c.Ctx, c, "main"); err == nil {
		t.Error("adopted unrelated attributed head")
	}
	stored, err := c.DB.GetRun(c.Run.ID)
	if err != nil || stored.HeadSHA != f.headSHA || c.Run.HeadSHA != f.headSHA {
		t.Fatalf("adopted head: %#v, %v", stored, err)
	}
	if got := gitCmd(t, f.dir, "rev-parse", "HEAD"); got != unrelated || !bytes.Equal(storedReplayMessage(t, f.dir, unrelated), original) {
		t.Fatal("validator rewrote unrelated commit")
	}
	gitCmd(t, f.dir, "reset", "--hard", f.headSHA)
	gitCmd(t, f.dir, "checkout", "-b", "actual-target")
	gitCmd(t, f.dir, "commit", "--allow-empty", "-m", "actual target\n\nCodex-Session: imported")
	target := gitCmd(t, f.dir, "rev-parse", "HEAD")
	message := storedReplayMessage(t, f.dir, target)
	gitCmd(t, f.dir, "checkout", "feature")
	for _, targets := range [][]string{{target}, {f.mainSHA}, nil} {
		if _, err := c.CommitAttributionEnv(nil, run, targets...); err != nil {
			t.Fatal(err)
		}
		if err := c.ValidateCommitAttribution(f.headSHA, target, run); err != nil {
			t.Fatal(err)
		}
		if err := c.ValidateCommitAttribution(f.headSHA, unrelated, run); err == nil {
			t.Error("refresh exempted unrelated history")
		}
	}
	if !bytes.Equal(storedReplayMessage(t, f.dir, target), message) {
		t.Fatal("imported bytes changed")
	}
}
