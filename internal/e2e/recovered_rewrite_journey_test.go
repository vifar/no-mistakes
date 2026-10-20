//go:build e2e

package e2e

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestAxiRecoveredRewriteRetiresMirrorJourney reproduces issue #97 with the
// real binary, an isolated daemon, the fake agent, and a local bare push
// target: a published branch is hand-rebased and force-pushed outside the
// pipeline, so the private gate mirror still holds history the rebase no
// longer contains. Before recovery `axi run` refuses exactly as it always
// has; `axi sync --recover` then anchors the superseded head in the gate and
// rebinds the push binding, and the next `axi run` starts a real run while
// naming the archive tag and the anchor that keep the retired commits.
func TestAxiRecoveredRewriteRetiresMirrorJourney(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: branchSyncScenario(t)})
	h.CommitChange("init-rewrite", "seed.txt", "seed\n", "seed rewrite init")
	initWorktree := h.AddWorktree("init-rewrite")
	if out, err := h.RunInDir(initWorktree, "init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}

	branch := "feature/rewrite-journey"
	submitted := h.CommitChange(branch, "feature.txt", "unsafe\n", "add unsafe feature")
	operator := h.AddWorktree(branch)
	if out, err := h.RunInDir(operator, "axi", "run", "--intent", "guard the feature value"); err != nil || !strings.Contains(out, "sync-1") {
		t.Fatalf("initial review gate: %v\n%s", err, out)
	}
	if out, err := h.RunInDir(operator, "axi", "respond", "--action", "fix", "--findings", "sync-1"); err != nil {
		t.Fatalf("review fix: %v\n%s", err, out)
	}
	if out, err := h.RunInDir(operator, "axi", "respond", "--action", "approve"); err != nil || !strings.Contains(out, "outcome: passed") {
		t.Fatalf("publish: %v\n%s", err, out)
	}
	published := h.UpstreamBranchSHA(branch)
	if published == submitted {
		t.Fatal("pipeline did not publish a fix commit")
	}
	if out, err := h.RunInDir(operator, "axi", "sync"); err != nil || !strings.Contains(out, "state: synchronized") {
		t.Fatalf("sync to the published head: %v\n%s", err, out)
	}

	gateDir := filepath.Join(h.NMHome, "repos", h.repoID()+".git")
	gitOut := func(dir string, args ...string) string {
		t.Helper()
		out, err := h.runGit(context.Background(), dir, args...)
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	mirror := gitOut(gateDir, "rev-parse", "refs/heads/"+branch)

	// The hand-rebase: same intent, rewritten history that no longer
	// contains the published pipeline commits, force-pushed outside the
	// pipeline.
	gitOut(operator, "reset", "--hard", submitted+"^")
	if err := os.WriteFile(filepath.Join(operator, "feature.txt"), []byte("safe, rebased by hand\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitOut(operator, "add", "feature.txt")
	gitOut(operator, "commit", "-m", "hand-rebased feature")
	rebased := gitOut(operator, "rev-parse", "HEAD")
	gitOut(operator, "push", "--force", h.UpstreamDir, "HEAD:refs/heads/"+branch)

	// Unrecovered: today's refusal, mirror untouched.
	refused, err := h.RunInDir(operator, "axi", "run", "--intent", "ship the hand-rebased feature")
	if err == nil {
		t.Fatalf("unrecovered rewrite started a run:\n%s", refused)
	}
	for _, want := range []string{"the private mirror for", "does not contain (mirror at " + mirror + "); it was left untouched"} {
		if !strings.Contains(refused, want) {
			t.Errorf("unrecovered refusal missing %q:\n%s", want, refused)
		}
	}
	if got := gitOut(gateDir, "rev-parse", "refs/heads/"+branch); got != mirror {
		t.Fatalf("refusal moved the mirror to %s", got)
	}

	check, _ := h.RunInDir(operator, "axi", "sync", "--check")
	if !strings.Contains(check, "code: recover_remote_rewritten") {
		t.Fatalf("check does not offer the rewrite recovery:\n%s", check)
	}
	recovered, err := h.RunInDir(operator, "axi", "sync", "--recover")
	if err != nil {
		t.Fatalf("recover: %v\n%s", err, recovered)
	}
	runs := h.Runs()
	var runID string
	for _, run := range runs {
		if run.Branch == branch {
			runID = run.ID
		}
	}
	anchor := "refs/no-mistakes/recover-rewritten/" + runID + "/"
	for _, want := range []string{"recovered: true", "source: remote_rewritten", anchor, "the push binding was rebound from " + published + " to " + rebased, "the private mirror refs/heads/" + branch + " at " + mirror + " is preserved by " + anchor} {
		if !strings.Contains(recovered, want) {
			t.Errorf("recover output missing %q:\n%s", want, recovered)
		}
	}
	anchors := gitOut(gateDir, "for-each-ref", "--format=%(refname) %(objectname)", "refs/no-mistakes/recover-rewritten/")
	if !strings.Contains(anchors, anchor) || !strings.Contains(anchors, published) {
		t.Fatalf("gate anchors = %q, want %s* at %s", anchors, anchor, published)
	}

	started, err := h.RunInDir(operator, "axi", "run", "--intent", "ship the hand-rebased feature")
	if err != nil {
		t.Fatalf("axi run after a recovered rewrite: %v\n%s", err, started)
	}
	archive := "refs/tags/no-mistakes-abandoned/" + branch + "/" + mirror
	for _, want := range []string{"retired the private mirror for", archive, "preserved by " + anchor, "gate:"} {
		if !strings.Contains(started, want) {
			t.Errorf("axi run output missing %q:\n%s", want, started)
		}
	}
	if got := gitOut(gateDir, "rev-parse", archive); got != mirror {
		t.Fatalf("archive %s = %s, want %s", archive, got, mirror)
	}
	if active := h.ActiveRun(branch); active == nil || active.ID == runID || active.HeadSHA != rebased {
		t.Fatalf("no fresh run for the hand-rebased head: %#v", active)
	}
}
