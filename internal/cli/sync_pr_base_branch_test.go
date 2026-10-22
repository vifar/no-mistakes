package cli

import (
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/branchsync"
	toon "github.com/toon-format/toon-go"
)

// The branch_sync object reports the lineage's base branch, so an operator
// reading a recovery handoff can see the review's subject.
func TestBranchSyncFieldCarriesTheLineageBaseBranch(t *testing.T) {
	state := branchsync.State{
		State:    branchsync.StateBehind,
		Local:    branchsync.LocalState{Branch: "feature/sync", Head: "abc", Clean: true},
		Pipeline: branchsync.PipelineState{RunID: "run-1", PRBaseBranch: "develop"},
	}
	rendered, err := toon.Marshal(branchSyncField(state))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(rendered), "pr_base_branch") {
		t.Fatalf("branch_sync does not report the base branch:\n%s", rendered)
	}
	if !strings.Contains(string(rendered), "develop") {
		t.Fatalf("branch_sync does not report the lineage's base branch:\n%s", rendered)
	}
}

// A lineage with no recorded base branch keeps the field out of the object,
// so an absent base is not confused with a recorded one.
func TestBranchSyncFieldOmitsAnUnsetBaseBranch(t *testing.T) {
	state := branchsync.State{
		State:    branchsync.StateBehind,
		Local:    branchsync.LocalState{Branch: "feature/sync", Head: "abc", Clean: true},
		Pipeline: branchsync.PipelineState{RunID: "run-1"},
	}
	rendered, err := toon.Marshal(branchSyncField(state))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(rendered), "pr_base_branch") {
		t.Fatalf("branch_sync reported a base branch the lineage never recorded:\n%s", rendered)
	}
}
