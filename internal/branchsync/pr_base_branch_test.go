package branchsync

import (
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// The lineage's base branch is part of what the review is about. An operator
// reading a recovery handoff must be able to see which base the branch is on
// and which base the next run will target.

// lineageRunWithBaseBranch records the branch's newest run with a per-run base
// branch, so the state under test carries a lineage base branch.
func lineageRunWithBaseBranch(t *testing.T, f *syncFixture, base string) *db.Run {
	t.Helper()
	run, err := f.db.InsertRunWithIntent(f.repo.ID, "feature/sync", f.old, f.base, nil, base)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.UpdateRunHeadSHA(run.ID, f.pushed); err != nil {
		t.Fatal(err)
	}
	if err := f.db.UpdateRunPushBinding(run.ID, db.PushBinding{
		HeadSHA: f.pushed, TargetKind: "upstream", TargetFingerprint: TargetFingerprint(f.remote), Ref: "refs/heads/feature/sync",
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.db.UpdateRunStatus(run.ID, types.RunCompleted); err != nil {
		t.Fatal(err)
	}
	return run
}

func TestInspectCachedCarriesTheLineageBaseBranch(t *testing.T) {
	f := newSyncFixture(t)
	lineageRunWithBaseBranch(t, f, "develop")

	state := f.service.InspectCached(f.ctx)
	if state.Pipeline.PRBaseBranch != "develop" {
		t.Fatalf("state.Pipeline.PRBaseBranch = %q, want develop", state.Pipeline.PRBaseBranch)
	}
}

func TestInspectCachedOmitsTheBaseBranchWhenTheLineageHasNone(t *testing.T) {
	f := newSyncFixture(t)

	state := f.service.InspectCached(f.ctx)
	if state.Pipeline.PRBaseBranch != "" {
		t.Fatalf("state.Pipeline.PRBaseBranch = %q, want empty", state.Pipeline.PRBaseBranch)
	}
}

// A recovery handoff must report the base the branch is on, so the operator
// and the next run can see the lineage's target instead of guessing it from
// the repository default.
func TestRecoverHandoffCarriesTheLineageBaseBranch(t *testing.T) {
	f := newSyncFixture(t)
	lineageRunWithBaseBranch(t, f, "develop")

	state := f.service.Recover(f.ctx, true)
	if state.Pipeline.PRBaseBranch != "develop" {
		t.Fatalf("recovery state.Pipeline.PRBaseBranch = %q, want develop", state.Pipeline.PRBaseBranch)
	}
}

func TestLineageHandoffSkipsStrictAndEmptyRecords(t *testing.T) {
	for _, action := range []string{"inspect", "recover"} {
		for _, source := range []string{"legacy", types.ShippingFull, types.ShippingImmutableSource} {
			for _, older := range []string{"", "develop"} {
				t.Run(action+"/"+source+"/"+older, func(t *testing.T) {
					f := newSyncFixture(t)
					if older != "" {
						lineageRunWithBaseBranch(t, f, older)
					}
					var shipping *types.ShippingOptions
					base := ""
					if source != "legacy" {
						base = "main"
						shipping = &types.ShippingOptions{Mode: source, TargetBranch: "main"}
					}
					run, err := f.db.InsertRunWithShipping(f.repo.ID, "feature/sync", f.old, f.base, nil, "", "", "", base, false, nil, shipping)
					if err != nil {
						t.Fatal(err)
					}
					if err := f.db.UpdateRunStatus(run.ID, types.RunCompleted); err != nil {
						t.Fatal(err)
					}
					state := f.service.InspectCached(f.ctx)
					if state.Pipeline.RunID != run.ID {
						t.Fatalf("selected run = %q, want %q", state.Pipeline.RunID, run.ID)
					}
					if action == "recover" {
						state = f.service.Recover(f.ctx, true)
					}
					if state.Pipeline.PRBaseBranch != older {
						t.Fatalf("handoff base = %q, want %q", state.Pipeline.PRBaseBranch, older)
					}
				})
			}
		}
	}
}
