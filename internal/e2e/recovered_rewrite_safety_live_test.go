//go:build e2e

package e2e

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
)

// The oracle is issue #97's preservation contract: only a committed recovery
// can authorize retirement, every retired commit stays reachable, and an
// invalid gate must not advance the binding. All decisions below go through
// the real CLI and daemon; Git only constructs adversarial repository state.
func TestRecoveredRewriteSafetyLiveJourney(t *testing.T) {
	t.Run("guards-and-safe-retry", func(t *testing.T) {
		j := newRewriteSafetyJourney(t)
		j.refusesRun("unrecovered rewrite")

		// A valid-looking leftover anchor is insufficient without a rebind.
		j.git(j.gate, "update-ref", j.anchor, j.published)
		j.refusesRun("anchor present but recovery never committed")
		j.git(j.gate, "update-ref", "-d", j.anchor)

		// An actual refused recovery may leave its worktree anchor behind.
		base := j.git(j.operator, "rev-parse", j.published+"~2")
		j.git(j.gate, "update-ref", j.anchor, base)
		j.refusesRecovery("conflicting gate anchor")
		j.refusesRun("refused recovery with a conflicting gate anchor")
		j.git(j.gate, "update-ref", "-d", j.anchor)

		for _, kind := range []string{"symlink-loop", "file", "not-repository", "checkout", "nested-checkout"} {
			saved := j.gate + ".saved"
			if err := os.Rename(j.gate, saved); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "symlink-loop":
				if err := os.Symlink(j.gate, j.gate); err != nil {
					t.Fatal(err)
				}
			case "file":
				if err := os.WriteFile(j.gate, []byte("not a gate\n"), 0600); err != nil {
					t.Fatal(err)
				}
			case "not-repository":
				if err := os.Mkdir(j.gate, 0700); err != nil {
					t.Fatal(err)
				}
			case "checkout":
				j.git(filepath.Dir(j.gate), "clone", saved, j.gate)
			case "nested-checkout":
				// j.gate is now an ordinary directory below another checkout,
				// so unrestricted Git discovery would write to that ancestor.
				j.git(j.h.NMHome, "init")
				if err := os.Mkdir(j.gate, 0700); err != nil {
					t.Fatal(err)
				}
			}
			j.refusesRecovery(kind + " gate")
			if kind == "nested-checkout" {
				if out, err := j.h.runGit(context.Background(), j.h.NMHome, "rev-parse", "--verify", j.anchor); err == nil {
					t.Fatalf("recovery wrote into ancestor checkout: %s", out)
				}
				if err := os.RemoveAll(filepath.Join(j.h.NMHome, ".git")); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.RemoveAll(j.gate); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(saved, j.gate); err != nil {
				t.Fatal(err)
			}
			j.refusesRun("after refused " + kind + " recovery")
		}

		recovered := j.cliOK("axi", "sync", "--recover")
		for _, want := range []string{"recovered: true", j.anchor, "the push binding was rebound from " + j.published + " to " + j.rebased, "the private mirror refs/heads/" + j.branch + " at " + j.mirror + " is preserved by " + j.anchor} {
			if !strings.Contains(recovered, want) {
				t.Fatalf("successful retry omitted %q:\n%s", want, recovered)
			}
		}
		if got := j.git(j.gate, "rev-parse", j.anchor); got != j.published {
			t.Fatalf("gate anchor = %s, want %s", got, j.published)
		}

		j.git(j.gate, "update-ref", "-d", j.anchor)
		// Symbolic, non-commit, malformed, future-generation, and unrelated
		// namespace anchors must not turn preservation into permission.
		for _, kind := range []string{"symbolic", "blob", "malformed", "future-generation", "unrelated-namespace"} {
			ref := j.anchor
			switch kind {
			case "symbolic":
				j.git(j.gate, "symbolic-ref", ref, "refs/heads/"+j.branch)
			case "blob":
				blob := j.git(j.gate, "rev-parse", j.published+":feature.txt")
				j.git(j.gate, "update-ref", ref, blob)
			case "malformed":
				ref += "x"
				j.git(j.gate, "update-ref", ref, j.published)
			case "future-generation":
				ref = j.anchor[:strings.LastIndex(j.anchor, "/")+1] + "999999"
				j.git(j.gate, "update-ref", ref, j.published)
			case "unrelated-namespace":
				ref = strings.Replace(j.anchor, "recover-rewritten/", "recover-rewrittenx/", 1)
				j.git(j.gate, "update-ref", ref, j.published)
			}
			j.refusesRun(kind + " anchor")
			j.git(j.gate, "update-ref", "--no-deref", "-d", ref)
		}

		j.git(j.gate, "update-ref", j.anchor, j.published)

		// Add genuine private work above the anchor. It must survive refusal.
		j.git(j.operator, "checkout", "--detach", j.mirror)
		if err := os.WriteFile(filepath.Join(j.operator, "late-private.txt"), []byte("never drop this\n"), 0600); err != nil {
			t.Fatal(err)
		}
		j.git(j.operator, "add", "late-private.txt")
		j.git(j.operator, "commit", "-m", "unpreserved private work")
		late := j.git(j.operator, "rev-parse", "HEAD")
		j.git(j.gate, "fetch", "--no-tags", j.operator, late)
		j.git(j.gate, "update-ref", "refs/heads/"+j.branch, late)
		j.git(j.operator, "checkout", j.branch)
		originalMirror := j.mirror
		j.mirror = late
		j.refusesRun("mirror contains work the committed recovery anchor misses")
		if got := j.git(j.gate, "show", late+":late-private.txt"); got != "never drop this" {
			t.Fatalf("private work changed: %q", got)
		}

		// Restore the fixture mirror, retaining the extra commit explicitly.
		j.git(j.gate, "update-ref", "refs/tags/live-test-extra-work", late)
		j.git(j.gate, "update-ref", "refs/heads/"+j.branch, originalMirror)
		j.mirror = originalMirror
		started := j.cliOK("axi", "run", "--intent", "ship the hand-rebased feature")
		archive := "refs/tags/no-mistakes-abandoned/" + j.branch + "/" + j.mirror
		for _, want := range []string{"retired the private mirror for", archive, "preserved by " + j.anchor, "gate:"} {
			if !strings.Contains(started, want) {
				t.Fatalf("fresh run omitted %q:\n%s", want, started)
			}
		}
		if got := j.git(j.gate, "rev-parse", archive); got != j.mirror {
			t.Fatalf("archive points to %s instead of %s", got, j.mirror)
		}
		if active := j.h.ActiveRun(j.branch); active == nil || active.ID == j.runID || active.HeadSHA != j.rebased {
			t.Fatalf("missing fresh run: %#v", active)
		}
		t.Logf("persisted archive %s -> %s; preserving anchor %s -> %s", archive, j.mirror, j.anchor, j.published)
	})

	t.Run("worktree-only-head-is-imported", func(t *testing.T) {
		j := newRewriteSafetyJourney(t)
		if err := os.Rename(j.gate, j.gate+".saved"); err != nil {
			t.Fatal(err)
		}
		j.git(filepath.Dir(j.gate), "init", "--bare", j.gate)
		if _, err := j.h.runGit(context.Background(), j.gate, "cat-file", "-e", j.published+"^{commit}"); err == nil {
			t.Fatal("empty replacement gate unexpectedly contains the published head")
		}
		out := j.cliOK("axi", "sync", "--recover")
		if !strings.Contains(out, "recovered: true") {
			t.Fatalf("worktree-only recovery failed:\n%s", out)
		}
		if got := j.git(j.gate, "rev-parse", j.anchor+"^{commit}"); got != j.published {
			t.Fatalf("imported anchor = %s, want %s", got, j.published)
		}
		t.Logf("previously absent gate object imported and pinned: %s -> %s", j.anchor, j.published)
	})
}

type rewriteSafetyJourney struct {
	t                                                                 *testing.T
	h                                                                 *Harness
	operator, gate, branch, published, mirror, rebased, runID, anchor string
	generation                                                        int64
}

func newRewriteSafetyJourney(t *testing.T) *rewriteSafetyJourney {
	t.Helper()
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: branchSyncScenario(t)})
	j := &rewriteSafetyJourney{t: t, h: h, branch: "feature/rewrite-safety"}
	h.CommitChange("init-rewrite-safety", "seed.txt", "seed\n", "seed rewrite init")
	if out, err := h.RunInDir(h.AddWorktree("init-rewrite-safety"), "init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	submitted := h.CommitChange(j.branch, "feature.txt", "unsafe\n", "add unsafe feature")
	j.operator = h.AddWorktree(j.branch)
	if out := j.cliOK("axi", "run", "--intent", "guard the feature value"); !strings.Contains(out, "sync-1") {
		t.Fatalf("initial review gate missing: %s", out)
	}
	j.cliOK("axi", "respond", "--action", "fix", "--findings", "sync-1")
	if out := j.cliOK("axi", "respond", "--action", "approve"); !strings.Contains(out, "outcome: passed") {
		t.Fatalf("publish did not finish: %s", out)
	}
	j.published = h.UpstreamBranchSHA(j.branch)
	if j.published == submitted {
		t.Fatal("pipeline did not publish a fix")
	}
	j.cliOK("axi", "sync")
	j.gate = filepath.Join(h.NMHome, "repos", h.repoID()+".git")
	j.mirror = j.git(j.gate, "rev-parse", "refs/heads/"+j.branch)
	for _, run := range h.Runs() {
		if run.Branch == j.branch {
			j.runID = run.ID
		}
	}
	run := j.record()
	if run.PushGeneration == nil {
		t.Fatal("published run has no push generation")
	}
	j.generation = *run.PushGeneration
	j.anchor = "refs/no-mistakes/recover-rewritten/" + j.runID + "/" + strconv.FormatInt(j.generation, 10)
	j.git(j.operator, "reset", "--hard", submitted+"^")
	if err := os.WriteFile(filepath.Join(j.operator, "feature.txt"), []byte("safe, rebased by hand\n"), 0644); err != nil {
		t.Fatal(err)
	}
	j.git(j.operator, "add", "feature.txt")
	j.git(j.operator, "commit", "-m", "hand-rebased feature")
	j.rebased = j.git(j.operator, "rev-parse", "HEAD")
	j.git(j.operator, "push", "--force", h.UpstreamDir, "HEAD:refs/heads/"+j.branch)
	return j
}

func (j *rewriteSafetyJourney) git(dir string, args ...string) string {
	j.t.Helper()
	out, err := j.h.runGit(context.Background(), dir, args...)
	if err != nil {
		j.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func (j *rewriteSafetyJourney) cli(args ...string) (string, error) {
	j.t.Helper()
	out, err := j.h.RunInDir(j.operator, args...)
	j.t.Logf("no-mistakes %s (error=%v):\n%s", strings.Join(args, " "), err, out)
	return out, err
}

func (j *rewriteSafetyJourney) cliOK(args ...string) string {
	j.t.Helper()
	out, err := j.cli(args...)
	if err != nil {
		j.t.Fatalf("CLI failed: %v\n%s", err, out)
	}
	return out
}

func (j *rewriteSafetyJourney) record() *db.Run {
	j.t.Helper()
	database, err := db.OpenReadOnly(paths.WithRoot(j.h.NMHome).DB())
	if err != nil {
		j.t.Fatal(err)
	}
	defer database.Close()
	run, err := database.GetRun(j.runID)
	if err != nil || run == nil {
		j.t.Fatalf("read published run: %v %#v", err, run)
	}
	return run
}

func (j *rewriteSafetyJourney) refusesRun(reason string) {
	j.t.Helper()
	out, err := j.cli("axi", "run", "--intent", "ship the hand-rebased feature")
	if err == nil || !strings.Contains(out, "the private mirror for") || !strings.Contains(out, "it was left untouched") {
		j.t.Fatalf("%s did not refuse the mirror: %v\n%s", reason, err, out)
	}
	if got := j.git(j.gate, "rev-parse", "refs/heads/"+j.branch); got != j.mirror {
		j.t.Fatalf("%s moved mirror to %s instead of %s", reason, got, j.mirror)
	}
	if active := j.h.ActiveRun(j.branch); active != nil {
		j.t.Fatalf("%s started a run: %#v", reason, active)
	}
	j.t.Logf("%s: mirror remains %s and no run started", reason, j.mirror)
}

func (j *rewriteSafetyJourney) refusesRecovery(reason string) {
	j.t.Helper()
	out, err := j.cli("axi", "sync", "--recover")
	if err == nil || !strings.Contains(out, "blocked_recover_preserve_failed") {
		j.t.Fatalf("%s did not refuse before rebind: %v\n%s", reason, err, out)
	}
	run := j.record()
	if run.LastPushedSHA == nil || *run.LastPushedSHA != j.published || run.PushGeneration == nil || *run.PushGeneration != j.generation {
		j.t.Fatalf("%s changed the binding: %#v", reason, run)
	}
	if got := j.git(j.operator, "rev-parse", "HEAD"); got != j.rebased {
		j.t.Fatalf("%s moved HEAD to %s", reason, got)
	}
	j.t.Logf("%s: binding remains %s at generation %d; worktree remains %s", reason, j.published, j.generation, j.rebased)
}
