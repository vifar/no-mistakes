//go:build e2e

package e2e

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/branchsync"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestAxiSyncConfirmsMergedPRAfterCIMonitorInterrupted(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude"})
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	branch := "feature/merged-after-interrupt"
	head := h.CommitChange(branch, "feature.txt", "feature\n", "feature")
	operator := h.AddWorktree(branch)
	database, err := db.Open(paths.WithRoot(h.NMHome).DB())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	run, err := database.InsertRun(h.repoID(), branch, head, head)
	if err != nil {
		t.Fatal(err)
	}
	// Model a real GitHub upstream while keeping all push I/O in the local fork.
	if _, err := database.UpdateRepoMetadataWithFork(run.RepoID, "https://github.com/example/repo.git", h.UpstreamDir, "main"); err != nil {
		t.Fatal(err)
	}
	for _, err := range []error{
		database.UpdateRunPushBinding(run.ID, db.PushBinding{HeadSHA: head, TargetKind: "fork", TargetFingerprint: branchsync.TargetFingerprint(h.UpstreamDir), Ref: "refs/heads/" + branch}),
		database.UpdateRunPRURL(run.ID, "https://github.com/example/repo/pull/42"),
		database.UpdateRunPRState(run.ID, "open"),
		database.UpdateRunErrorStatus(run.ID, types.RunCIMonitorInterruptedReason, types.RunCIMonitorInterrupted),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}
	// The branch was published, merged, and deleted while CI was no longer
	// monitoring. The fake forge, rather than the stored row, knows it merged.
	if out, err := h.runGit(context.Background(), h.WorkDir, "push", h.UpstreamDir, head+":refs/heads/"+branch); err != nil {
		t.Fatalf("publish: %v\n%s", err, out)
	}
	if out, err := h.runGit(context.Background(), h.UpstreamDir, "update-ref", "-d", "refs/heads/"+branch); err != nil {
		t.Fatalf("delete merged remote branch: %v\n%s", err, out)
	}
	statePath := filepath.Join(t.TempDir(), "gh-state.json")
	if err := os.WriteFile(statePath, []byte(`{"prs":{"feature/merged-after-interrupt":{"number":42,"url":"https://github.com/example/repo/pull/42","head":"feature/merged-after-interrupt","base":"main","state":"MERGED"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKEAGENT_GH_MODE", "stateful-pr")
	t.Setenv("FAKEAGENT_GH_STATE", statePath)
	refsBefore, err := h.runGit(context.Background(), operator, "show-ref")
	if err != nil {
		t.Fatal(err)
	}
	for _, check := range []bool{true, false} {
		args := []string{"axi", "sync"}
		if check {
			args = append(args, "--check")
		}
		out, err := h.RunInDir(operator, args...)
		if err != nil || !strings.Contains(out, "state: merged_remote_removed") || !strings.Contains(out, "safety: already_retired") {
			t.Fatalf("sync (check=%v): %v\n%s", check, err, out)
		}
		observed, err := database.GetRun(run.ID)
		if err != nil {
			t.Fatal(err)
		}
		want := "merged"
		if check {
			want = "open"
		}
		if observed.PRState == nil || *observed.PRState != want || observed.Status != types.RunCIMonitorInterrupted {
			t.Fatalf("sync (check=%v) run = %+v, want PR state %s and unchanged terminal status", check, observed, want)
		}
		refsAfter, err := h.runGit(context.Background(), operator, "show-ref")
		if err != nil || string(refsAfter) != string(refsBefore) || strings.TrimSpace(h.WorktreeRefSHA(branch)) != head {
			t.Fatalf("sync (check=%v) changed Git refs or HEAD: %v", check, err)
		}
	}
}
