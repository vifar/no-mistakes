package steps

import (
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// The base branch is part of what the review is about. A run that carries its
// lineage's base branch resolves it from its own record, never from a
// fallback, so a successor run reviews the same object the lineage reviewed.

// An inherited base branch is recorded on the run, and the run resolves it
// from that record. The inherited value carries no retarget authority, so it
// is resolved exactly like any other recorded base branch.
func TestEffectivePRBaseBranch_ResolvesTheRecordedLineageBaseBranch(t *testing.T) {
	t.Parallel()
	runBase := "develop"
	sctx := &pipeline.StepContext{
		Run:    &db.Run{PRBaseBranch: &runBase, PRBaseBranchRequested: false},
		Repo:   &db.Repo{DefaultBranch: "main"},
		Config: &config.Config{PR: config.PR{BaseBranch: "trunk"}},
	}
	if got := effectivePRBaseBranch(sctx); got != runBase {
		t.Fatalf("effectivePRBaseBranch = %q, want the recorded %q", got, runBase)
	}
}

// With no lineage record and no per-run override, the repository's own
// default branch is what the run targets.
func TestEffectivePRBaseBranch_NoLineageNoOverrideUsesTheRepositoryDefault(t *testing.T) {
	t.Parallel()
	sctx := &pipeline.StepContext{
		Run:    &db.Run{},
		Repo:   &db.Repo{DefaultBranch: "develop"},
		Config: &config.Config{},
	}
	if got := effectivePRBaseBranch(sctx); got != "develop" {
		t.Fatalf("effectivePRBaseBranch = %q, want the repository default develop", got)
	}
}

// The hardcoded "main" is a last resort for a repository whose default
// branch is unknown, and reaching it is recorded rather than applied
// silently.
func TestEffectivePRBaseBranch_LastResortIsRecordedNotSilent(t *testing.T) {
	t.Parallel()
	var logged []string
	sctx := &pipeline.StepContext{
		Run:    &db.Run{},
		Repo:   &db.Repo{},
		Config: &config.Config{},
		Log:    func(line string) { logged = append(logged, line) },
	}
	if got := effectivePRBaseBranch(sctx); got != "main" {
		t.Fatalf("effectivePRBaseBranch = %q, want the last resort main", got)
	}
	if len(logged) == 0 {
		t.Fatal("the last-resort base branch was applied without a record")
	}
	if !strings.Contains(strings.Join(logged, "\n"), "main") {
		t.Fatalf("the record does not name the guessed base branch: %v", logged)
	}
}

// A repository whose own default branch is known never reaches the "main"
// literal, and never records a fallback.
func TestEffectivePRBaseBranch_KnownDefaultNeverRecordsAFallback(t *testing.T) {
	t.Parallel()
	var logged []string
	sctx := &pipeline.StepContext{
		Run:    &db.Run{},
		Repo:   &db.Repo{DefaultBranch: "master"},
		Config: &config.Config{},
		Log:    func(line string) { logged = append(logged, line) },
	}
	if got := effectivePRBaseBranch(sctx); got != "master" {
		t.Fatalf("effectivePRBaseBranch = %q, want the repository default master", got)
	}
	if len(logged) != 0 {
		t.Fatalf("a resolved default branch produced a fallback record: %v", logged)
	}
}

// An inherited base branch is a fact about the review, not a fresh request.
// It never retargets the pull request: only the run that asked for a retarget
// carries that one-time authority.
func TestResolveAndApplyPRTarget_AnInheritedBaseBranchNeverRetargets(t *testing.T) {
	sctx, facts := selectionFixture(t)
	sctx.Run.PRURL = &facts.PR.URL
	sctx.Run.PRBaseBranch = strptr("epic/feature")
	sctx.Run.PRBaseBranchRequested = false
	reader := &fakePRFactsReader{read: facts}

	selection, freshRetarget, err := resolveAndApplyPRTarget(sctx, reader, reader, false)
	if err != nil {
		t.Fatal(err)
	}
	if freshRetarget {
		t.Fatal("an inherited base branch retargeted the pull request")
	}
	if len(reader.retargets) != 0 {
		t.Fatalf("retargets = %v; an inherited base branch must not retarget", reader.retargets)
	}
	if selection.TargetBranch != facts.BaseBranch {
		t.Fatalf("selection target = %q, want the live PR target %q", selection.TargetBranch, facts.BaseBranch)
	}
}

// Strict shipping is unchanged: the pinned target still wins over every other
// source, including a recorded base branch.
func TestEffectivePRBaseBranch_StrictShippingPinnedTargetStillWins(t *testing.T) {
	t.Parallel()
	runBase := "develop"
	sctx := &pipeline.StepContext{
		Run: &db.Run{
			PRBaseBranch: &runBase,
			Shipping:     &types.ShippingOptions{Mode: types.ShippingFull, TargetBranch: "release/1.x"},
		},
		Repo:   &db.Repo{DefaultBranch: "main"},
		Config: &config.Config{PR: config.PR{BaseBranch: "trunk"}},
	}
	if got := effectivePRBaseBranch(sctx); got != "release/1.x" {
		t.Fatalf("effectivePRBaseBranch = %q, want the pinned strict target release/1.x", got)
	}
}
