package steps

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// controllerDocumentLimit is the cap the stuck controller run should have
// used. The root's repository_overrides set it to 6 for the upstream URL.
// The run recorded commit counts 1, 0, 0, 1 and stopped at Document.
func controllerDocumentConfig(t *testing.T, limit int) *config.Config {
	t.Helper()
	global, err := config.LoadGlobalFromBytes([]byte(
		"repository_overrides:\n  https://github.com/vifar/controller.git:\n    document:\n      max_commits_per_run: " +
			strconv.Itoa(limit) + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	repo, err := config.LoadRepoFromBytes([]byte("commands:\n  lint: 'true'\n"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.MergeForRemote(global, config.EffectiveRepoConfig(repo, repo, false), "https://github.com/vifar/controller.git")
	if cfg.Commands.Lint != "true" {
		t.Fatal("lint command was dropped")
	}
	return cfg
}

func recordDocumentCommitCounts(t *testing.T, sc *pipeline.StepContext, counts ...int) {
	t.Helper()
	const findings = `{"findings":[],"summary":"recorded pass"}`
	for _, count := range counts {
		id, err := sc.DB.BeginDocumentPass(sc.Run.ID, sc.Run.HeadSHA)
		if err != nil {
			t.Fatal(err)
		}
		if err := sc.DB.CompleteDocumentPass(id, sc.Run.HeadSHA, count, findings); err != nil {
			t.Fatal(err)
		}
	}
}

// documentPurposeAgent records why it was invoked. An editing pass writes one
// documentation file. A gap check does not.
type documentPurposeAgent struct {
	t        *testing.T
	dir      string
	purposes []string
}

func (a *documentPurposeAgent) Name() string { return "test" }

func (a *documentPurposeAgent) Run(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
	a.purposes = append(a.purposes, opts.Purpose)
	if opts.Purpose == "document-gap-check" {
		return &agent.Result{Output: json.RawMessage(`{"findings":[{"severity":"warning","description":"the rollout guide is still stale","action":"ask-user"}],"summary":"gap remains"}`)}, nil
	}
	if err := os.WriteFile(filepath.Join(a.dir, "README.md"), []byte("guide\n"), 0o644); err != nil {
		a.t.Fatal(err)
	}
	return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"update guide"}`)}, nil
}

func (a *documentPurposeAgent) Close() error { return nil }

func TestDocumentRunWithTwoCommitsUsesConfiguredOverride(t *testing.T) {
	dir, base, head := setupGitRepo(t)
	ag := &documentPurposeAgent{t: t, dir: dir}
	sc := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{Lint: "true"})
	sc.Config = controllerDocumentConfig(t, 6)
	sc.Repo.UpstreamURL = "https://github.com/vifar/controller.git"
	// The parked run's document_passes: two commits and two empty passes.
	// A later code commit moves the head, so the exact-head reuse does not
	// hide the cap check.
	recordDocumentCommitCounts(t, sc, 1, 0, 0, 1)
	advanceHead(t, &pipelineStepContext{dir: dir, sctx: sc}, "feature.txt", "reviewed repair\n")
	history, err := readDocumentHistory(sc)
	if err != nil {
		t.Fatal(err)
	}
	if history.commits != 2 {
		t.Fatalf("recorded commits = %d, want 2", history.commits)
	}
	if sc.Config.Document.MaxCommitsPerRun != 6 {
		t.Fatalf("effective limit = %d, want 6; the default of %d is what stopped the run", sc.Config.Document.MaxCommitsPerRun, config.DefaultDocumentMaxCommitsPerRun)
	}

	if _, err := (&DocumentStep{}).Execute(sc); err != nil {
		t.Fatal(err)
	}
	if len(ag.purposes) != 1 || ag.purposes[0] != "document" {
		t.Fatalf("agent purposes = %v, want one editing document pass (2 commits is under the override of 6)", ag.purposes)
	}
}

func TestDocumentCommitComparisonUsesTheConfiguredLimit(t *testing.T) {
	for _, tc := range []struct {
		name    string
		commits int
		want    string
	}{
		{"one under the cap still edits", 5, "document"},
		{"the cap itself stops before another edit", 6, "document-gap-check"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, base, head := setupGitRepo(t)
			ag := &documentPurposeAgent{t: t, dir: dir}
			sc := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{Lint: "true"})
			sc.Config = controllerDocumentConfig(t, 6)
			counts := make([]int, tc.commits)
			for i := range counts {
				counts[i] = 1
			}
			recordDocumentCommitCounts(t, sc, counts...)
			advanceHead(t, &pipelineStepContext{dir: dir, sctx: sc}, "feature.txt", "reviewed repair\n")
			if _, err := (&DocumentStep{}).Execute(sc); err != nil {
				t.Fatal(err)
			}
			if len(ag.purposes) != 1 || ag.purposes[0] != tc.want {
				t.Fatalf("agent purposes = %v, want %s", ag.purposes, tc.want)
			}
		})
	}
}

func TestNewRunStartsWithAFreshDocumentCommitCount(t *testing.T) {
	dir, base, head := setupGitRepo(t)
	ag := &documentPurposeAgent{t: t, dir: dir}
	sc := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{Lint: "true"})
	sc.Config.Document.MaxCommitsPerRun = config.DefaultDocumentMaxCommitsPerRun
	sc.Run.Shipping = &types.ShippingOptions{Mode: types.ShippingFull}
	recordDocumentCommitCounts(t, sc, 1, 1)
	advanceHead(t, &pipelineStepContext{dir: dir, sctx: sc}, "feature.txt", "reviewed repair\n")

	out, err := (&DocumentStep{}).Execute(sc)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Findings, "Abort this run and start a new one") || !strings.Contains(out.Findings, "fresh document commit count") {
		t.Fatalf("cap findings do not tell the operator to start a new run: %s", out.Findings)
	}

	fresh, err := sc.DB.InsertRun(sc.Repo.ID, "refs/heads/feature-next", sc.Run.HeadSHA, sc.Run.BaseSHA)
	if err != nil {
		t.Fatal(err)
	}
	fresh.Shipping = sc.Run.Shipping
	// The capped run's gap check pinned this worktree's attribution boundary
	// to that run's submitted head. A new run is its own checkout, so it does
	// not inherit the boundary and can commit against its own submitted head.
	meta := gitCmd(t, dir, "rev-parse", "--absolute-git-dir")
	if resolved, err := filepath.EvalSymlinks(meta); err != nil {
		t.Fatal(err)
	} else {
		meta = resolved
	}
	if err := os.RemoveAll(filepath.Join(meta, "no-mistakes-attribution")); err != nil {
		t.Fatal(err)
	}
	nextAgent := &documentPurposeAgent{t: t, dir: dir}
	next := *sc
	next.Run = fresh
	next.Agent = nextAgent
	history, err := readDocumentHistory(&next)
	if err != nil {
		t.Fatal(err)
	}
	if history.commits != 0 {
		t.Fatalf("new run commit count = %d, want a fresh 0", history.commits)
	}
	if _, err := (&DocumentStep{}).Execute(&next); err != nil {
		t.Fatal(err)
	}
	if len(nextAgent.purposes) != 1 || nextAgent.purposes[0] != "document" {
		t.Fatalf("new run agent purposes = %v, want an editing pass", nextAgent.purposes)
	}
}
