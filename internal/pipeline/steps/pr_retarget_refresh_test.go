package steps

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// sequencedPRFactsReader answers ReadPRFacts from a script: each read takes
// the next head (or error); the last head repeats. A retarget moves the base
// of every later answer, like the forge would.
type sequencedPRFactsReader struct {
	facts     scm.PRFacts
	heads     []string
	errs      []error
	readCalls int
	retargets []string
}

func (r *sequencedPRFactsReader) ReadPRFacts(_ context.Context, _ *scm.PR) (scm.PRFacts, error) {
	i := r.readCalls
	r.readCalls++
	if i < len(r.errs) && r.errs[i] != nil {
		return scm.PRFacts{}, r.errs[i]
	}
	facts := r.facts
	if len(r.heads) > 0 {
		facts.HeadSHA = r.heads[min(i, len(r.heads)-1)]
	}
	return facts, nil
}

func (r *sequencedPRFactsReader) FindOpenPRFacts(context.Context, string, string) ([]scm.PRFacts, error) {
	return nil, errors.New("owned PR must be read by identity")
}

func (r *sequencedPRFactsReader) SetPRBaseBranch(_ context.Context, _ *scm.PR, branch string) error {
	r.retargets = append(r.retargets, branch)
	r.facts.BaseBranch = branch
	return nil
}

type retargetRefresh struct {
	t       *testing.T
	sctx    *pipeline.StepContext
	reader  *sequencedPRFactsReader
	local   string
	foreign string
}

func retargetRefreshFixture(t *testing.T) *retargetRefresh {
	t.Helper()
	sctx, facts := selectionFixture(t)
	sctx.Run.PRURL = &facts.PR.URL
	sctx.Run.PRBaseBranch = strptr("release")
	sctx.Run.PRBaseBranchRequested = true
	ensureLocalBranch(t, sctx.WorkDir, "develop", sctx.Run.BaseSHA)
	ensureLocalBranch(t, sctx.WorkDir, "release", sctx.Run.BaseSHA)
	// A real commit that no line of this run's history contains.
	gitCmd(t, sctx.WorkDir, "checkout", "-q", "-b", "foreign", sctx.Run.BaseSHA)
	gitCmd(t, sctx.WorkDir, "commit", "-q", "--allow-empty", "-m", "foreign push")
	foreign := gitCmd(t, sctx.WorkDir, "rev-parse", "HEAD")
	gitCmd(t, sctx.WorkDir, "checkout", "-q", "feature")
	return &retargetRefresh{t: t, sctx: sctx, reader: &sequencedPRFactsReader{facts: facts}, local: sctx.Run.HeadSHA, foreign: foreign}
}

// run applies the fresh retarget. On success it also pins the Rebase-entry
// receipt and proves it records the refreshed forge head, not the stale one.
func (f *retargetRefresh) run() (string, error) {
	t, sctx, reader := f.t, f.sctx, f.reader
	t.Helper()
	selection, fresh, err := resolveAndApplyPRTarget(sctx, reader, reader, false)
	if err != nil {
		if len(reader.retargets) != 0 || !sctx.Run.PRBaseBranchRequested {
			t.Fatalf("failed retarget mutated the PR or consumed the request: retargets=%v requested=%v", reader.retargets, sctx.Run.PRBaseBranchRequested)
		}
		return "", err
	}
	if selection.TargetBranch != "release" || len(reader.retargets) != 1 || sctx.Run.PRBaseBranchRequested {
		t.Fatalf("selection=%+v retargets=%v requested=%v", selection, reader.retargets, sctx.Run.PRBaseBranchRequested)
	}
	if _, err := guardPRContextWithSelection(sctx, types.StepRebase, selection, fresh); err != nil {
		t.Fatal(err)
	}
	receipt, err := sctx.DB.GetRunPRContext(sctx.Run.ID)
	if err != nil || receipt == nil {
		t.Fatalf("receipt=%+v err=%v", receipt, err)
	}
	if receipt.ForgeHeadSHA != selection.ForgeHeadSHA {
		t.Fatalf("receipt forge head %s, refreshed selection %s", receipt.ForgeHeadSHA, selection.ForgeHeadSHA)
	}
	return selection.ForgeHeadSHA, nil
}

func TestFreshRetargetRefreshesStalePRHeadEqualToLocal(t *testing.T) {
	f := retargetRefreshFixture(t)
	f.reader.heads = []string{f.foreign, f.local}
	got, err := f.run()
	if err != nil {
		t.Fatalf("stale PR head blocked the retarget: %v", err)
	}
	if got != f.local {
		t.Fatalf("recorded forge head = %s, want refreshed local head %s", got, f.local)
	}
}

func TestFreshRetargetAcceptsPRHeadTheLocalHeadContains(t *testing.T) {
	f := retargetRefreshFixture(t)
	f.reader.heads = []string{f.sctx.Run.BaseSHA}
	got, err := f.run()
	if err != nil {
		t.Fatalf("ancestor PR head blocked the retarget: %v", err)
	}
	if got != f.sctx.Run.BaseSHA {
		t.Fatalf("recorded forge head = %s, want %s", got, f.sctx.Run.BaseSHA)
	}
}

func TestFreshRetargetAcceptsTheRunsOwnLastPush(t *testing.T) {
	f := retargetRefreshFixture(t)
	// The run published a rewritten head (not an ancestor of the local
	// head); its durable publication record accounts for it.
	if err := f.sctx.DB.UpdateRunPushBinding(f.sctx.Run.ID, db.PushBinding{HeadSHA: f.foreign, TargetKind: "origin", Ref: "refs/heads/feature"}); err != nil {
		t.Fatal(err)
	}
	f.reader.heads = []string{f.foreign}
	got, err := f.run()
	if err != nil {
		t.Fatalf("the run's own push blocked the retarget: %v", err)
	}
	if got != f.foreign {
		t.Fatalf("recorded forge head = %s, want %s", got, f.foreign)
	}
}

func TestFreshRetargetKeepsFailingOnForeignPRHead(t *testing.T) {
	f := retargetRefreshFixture(t)
	f.reader.heads = []string{f.foreign}
	_, err := f.run()
	if err == nil {
		t.Fatal("expected a foreign PR head to fail the retarget")
	}
	if !strings.Contains(err.Error(), f.foreign) || !strings.Contains(err.Error(), f.local) || !strings.Contains(err.Error(), "before retarget") {
		t.Fatalf("error %q must name the PR head %s and local head %s", err, f.foreign, f.local)
	}
	if f.reader.readCalls != 2 {
		t.Fatalf("read calls = %d, want the first read plus one refresh", f.reader.readCalls)
	}
}

func TestFreshRetargetRefreshReadIsRetriedOnceThenFails(t *testing.T) {
	readErr := errors.New("gh api: HTTP 502")
	t.Run("retry recovers", func(t *testing.T) {
		f := retargetRefreshFixture(t)
		f.reader.heads = []string{f.foreign, f.foreign, f.local}
		f.reader.errs = []error{nil, readErr}
		if _, err := f.run(); err != nil {
			t.Fatalf("one failed refresh read must be retried: %v", err)
		}
	})
	t.Run("retry fails", func(t *testing.T) {
		f := retargetRefreshFixture(t)
		f.reader.heads = []string{f.foreign}
		f.reader.errs = []error{nil, readErr, readErr}
		_, err := f.run()
		if err == nil {
			t.Fatal("a failed refresh must never pass silently")
		}
		if !strings.Contains(err.Error(), "refresh pull request head before retarget") || !errors.Is(err, readErr) ||
			!strings.Contains(err.Error(), f.foreign) || !strings.Contains(err.Error(), f.local) {
			t.Fatalf("error %q must say the refresh failed and name both heads", err)
		}
		if f.reader.readCalls != 3 {
			t.Fatalf("read calls = %d, want first read, refresh and one retry", f.reader.readCalls)
		}
	})
}
