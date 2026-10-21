//go:build e2e

package e2e

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/custody"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Exercise the real CLI/daemon/Git/SQLite boundary with an owned legacy row.
// No forge is configured: custody recovery must not invent publication.
func TestFailedPushRecoveryJourney(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: axiScenario(t)})
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	branch := "feature/failed-push"
	submitted := h.CommitChange(branch, "feature.txt", "operator work\n", "operator change")
	operator := h.AddWorktree(branch)
	gate := filepath.Join(h.NMHome, "repos", h.repoID()+".git")
	git := func(dir string, args ...string) string {
		t.Helper()
		out, err := h.runGit(context.Background(), dir, args...)
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	// Fetch seeds only the fixture gate; unlike a push it cannot launch a run.
	git(gate, "fetch", operator, submitted+":refs/heads/"+branch)
	pipelineDir := filepath.Join(t.TempDir(), "pipeline")
	git(gate, "worktree", "add", "--detach", pipelineDir, submitted)
	if err := os.WriteFile(filepath.Join(pipelineDir, "pipeline-fix.txt"), []byte("preserved pipeline change\n"), 0644); err != nil {
		t.Fatal(err)
	}
	git(pipelineDir, "add", "pipeline-fix.txt")
	git(pipelineDir, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "pipeline fix")
	preserved := git(pipelineDir, "rev-parse", "HEAD")
	git(gate, "update-ref", "refs/heads/"+branch, preserved, submitted)
	git(gate, "worktree", "remove", pipelineDir)
	d, err := db.Open(paths.WithRoot(h.NMHome).DB())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	run, err := d.InsertRun(h.repoID(), branch, submitted, git(operator, "rev-parse", submitted+"^"))
	if err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateRunErrorStatusWithVerifiedHead(run.ID, "check PR context after push: PR comparison changed after review", types.RunFailed, preserved); err != nil {
		t.Fatal(err)
	}
	git(gate, "update-ref", custody.RecoveryRef(run.ID), preserved)
	step, err := d.InsertStepResult(run.ID, types.StepPush)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.StartStep(step.ID); err != nil {
		t.Fatal(err)
	}
	assertUnpublished := func() {
		t.Helper()
		stored, err := d.GetRun(run.ID)
		if err != nil {
			t.Fatal(err)
		}
		if stored.PushActive || stored.LastPushedSHA != nil || stored.PRURL != nil || stored.PushGeneration != nil {
			t.Fatalf("invented publication: %+v", stored)
		}
		publications, err := d.GetCommitStatusPublications(context.Background(), run.ID)
		if err != nil || len(publications) != 0 {
			t.Fatalf("invented status: %+v %v", publications, err)
		}
		if remote := git(h.UpstreamDir, "for-each-ref", "refs/heads/"+branch); remote != "" {
			t.Fatalf("invented remote branch: %s", remote)
		}
	}
	assertUnpublished()
	// All four surfaces used to refuse or point back at status. Keep the exact
	// before-recovery output as evidence; rerun's head mismatch remains valid.
	for _, args := range [][]string{{"axi", "status"}, {"axi", "abort"}, {"rerun"}, {"axi", "run", "--intent", "retry failed push"}} {
		out, err := h.RunInDir(operator, args...)
		t.Logf("before recovery %v (error=%v):\n%s", args, err, out)
		if len(args) > 1 && args[0] == "axi" && args[1] == "status" {
			if err != nil || !strings.Contains(out, "code: recover_custody") || strings.Contains(out, "code: continue_active_run") {
				t.Errorf("status has no supported custody exit:\n%s", out)
			}
		}
	}
	out, err := h.RunInDir(operator, "axi", "sync", "--recover")
	t.Logf("supported recovery:\n%s", out)
	if err != nil || !strings.Contains(out, "recovered: true") {
		t.Fatalf("recovery: %v\n%s", err, out)
	}
	if head := git(operator, "rev-parse", "HEAD"); head != preserved {
		t.Fatalf("recovery head=%s want %s", head, preserved)
	}
	if content, err := os.ReadFile(filepath.Join(operator, "pipeline-fix.txt")); err != nil || string(content) != "preserved pipeline change\n" {
		t.Fatalf("lost fix: %q %v", content, err)
	}
	assertUnpublished()
	out, err = h.RunInDir(operator, "axi", "run", "--intent", "validate recovered failed-push commits")
	t.Logf("fresh supported run:\n%s", out)
	if err != nil || !strings.Contains(out, "gate:") {
		t.Fatalf("fresh run refused: %v\n%s", err, out)
	}
	// The fixture intentionally parks Review; harness cleanup owns its daemon.
	fresh := h.ActiveRun(branch)
	if fresh == nil || fresh.ID == run.ID {
		t.Fatalf("fresh run not created: %+v", fresh)
	}
}
