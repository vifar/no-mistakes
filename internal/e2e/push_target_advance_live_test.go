//go:build e2e && !windows

package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps"
	"github.com/kunchenguid/no-mistakes/internal/testgit"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// The real daemon and CLI publish to a disposable Git remote. Only the
// external agent/forge are simulated. The Git wrapper delegates every command
// to real Git, moving the remote exactly when Push reads it. Git ancestry,
// persisted rewind history and the published PR body are independent oracles:
// a terminal failure, stale publication or silent guard bypass must fail.
func TestPushTargetAdvanceLiveJourney(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is needed for the disposable remote's deterministic race")
	}
	realGit, err := testgit.RealGit()
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"push read", "attestation read", "conflict", "rewritten target", "unreadable target", "changed PR", "changed head"} {
		t.Run(kind, func(t *testing.T) {
			j := newScopeJourney(t, SetupOpts{Agent: "claude", Scenario: cleanReviewScenario(t)})
			h, state := j.h, j.statePath
			j.setForge(forgeState{Checks: []forgeCheck{greenCheck("build")}})
			commitTrustedRepoConfig(t, h, "commands:\n  test: 'true'\n  lint: 'true'\nauto_fix:\n  rebase: 0\n")
			base := h.UpstreamBranchSHA("main")
			if kind == "rewritten target" {
				gitIn(t, h, h.WorkDir, "reset", "--hard", base+"^")
			}
			path := "landed.txt"
			if kind == "conflict" {
				path = "feature.txt"
			}
			target := h.CommitChange("main", path, "landed target\n", "prepare target")
			gitIn(t, h, h.WorkDir, "push", "origin", target+":refs/heads/prepared-target")
			gitIn(t, h, h.WorkDir, "reset", "--hard", base)
			const branch = "feature/push-window"
			source := h.CommitChange(branch, "feature.txt", "submitted feature\n", "published source")
			gitIn(t, h, h.WorkDir, "push", "origin", branch)
			head := h.CommitChange(branch, "unpublished.txt", "local addition\n", "unpublished addition")
			body, _ := steps.BuildPipelineSummary([]*db.StepResult{{StepName: types.StepReview, Status: types.StepStatusCompleted}, {StepName: types.StepTest, Status: types.StepStatusCompleted}, {StepName: types.StepDocument, Status: types.StepStatusCompleted}}, nil, source)
			writeStatefulGH(t, state, statefulGHState{PRs: map[string]*statefulGHPR{branch: {Number: 42, URL: "https://github.com/example/closes/pull/42", Title: "Push window", Body: body, Base: "main", Head: branch, State: "OPEN"}}})
			root := filepath.Dir(h.AgentLog)
			trace := filepath.Join(root, "push-fetches.log")
			trigger := 1
			if kind == "attestation read" || kind == "unreadable target" {
				trigger = 2
			}
			script := fmt.Sprintf(pushTargetRacePython, realGit, h.NMHome, trace, trigger, kind, h.UpstreamDir, target, state, branch)
			driver := filepath.Join(root, "race.py")
			if err := os.WriteFile(driver, []byte(script), 0600); err != nil {
				t.Fatal(err)
			}
			wrapper := "#!/bin/sh\ncase \" $* \" in\n  *' fetch '*'+refs/heads/main:refs/remotes/origin/main'*) exec " + shellQuote(python) + " " + shellQuote(driver) + " \"$@\" ;;\nesac\nexec " + shellQuote(realGit) + " \"$@\"\n"
			if err := os.WriteFile(filepath.Join(h.BinDir, "git"), []byte(wrapper), 0700); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if _, err := os.Stat(h.UpstreamDir + ".offline"); err == nil {
					_ = os.Rename(h.UpstreamDir+".offline", h.UpstreamDir)
				}
			})
			initFromOwnWorktree(t, h, "init-push-window")
			h.Checkout(branch)
			h.PushToGate(branch)
			if kind == "conflict" {
				run := waitForStepStatus(t, h, branch, types.StepRebase, types.StepStatusAwaitingApproval, 90*time.Second)
				if run.HeadSHA != head || h.UpstreamBranchSHA(branch) != source {
					t.Fatal("conflicting rewind changed the submitted or published head")
				}
				logTargetRewindStatus(t, h, run.ID)
			} else {
				var run *ipc.RunInfo
				if kind == "push read" || kind == "attestation read" {
					run = j.expectReady(branch, 30*time.Second)
				} else {
					run = h.WaitForRun(branch, 90*time.Second)
				}
				if kind == "push read" || kind == "attestation read" {
					for _, name := range []types.StepName{types.StepPush, types.StepPR} {
						step, ok := findStep(run.Steps, name)
						if !ok || step.Status != types.StepStatusCompleted {
							t.Fatalf("target movement did not finish %s: %+v", name, step)
						}
					}
					published := h.UpstreamBranchSHA(branch)
					if published != run.HeadSHA || !isAncestorIn(t, h, h.UpstreamDir, target, published) {
						t.Fatal("published head did not integrate the advanced target")
					}
					if got := gitIn(t, h, h.UpstreamDir, "show", published+":unpublished.txt"); got != "local addition" {
						t.Fatalf("submitted work lost: %q", got)
					}
					charge, pending, history := targetRewindHistory(t, h.NMHome, run.ID)
					if charge != 1 || pending || history != 1 {
						t.Fatalf("rewind lifecycle = %d %t %d", charge, pending, history)
					}
					body := livePRBody(t, state, branch)
					if !strings.Contains(body, published) {
						t.Fatalf("PR body did not attest the published head: %s", body)
					}
					logTargetRewindStatus(t, h, run.ID)
					t.Logf("published=%s contains advanced target=%s and submitted work; live PR body:\n%s", published, target, body)
				} else {
					if run.Status != types.RunFailed || run.Error == nil {
						t.Fatalf("unsafe comparison did not fail closed: %s %s", run.Status, deref(run.Error))
					}
					database, err := db.OpenReadOnly(filepath.Join(h.NMHome, "state.sqlite"))
					if err != nil {
						t.Fatal(err)
					}
					stored, err := database.GetRun(run.ID)
					_ = database.Close()
					if err != nil || stored.LastPushedSHA != nil || stored.TargetAdvanceRewinds != 0 {
						t.Fatalf("unsafe comparison rewound or published: %+v %v", stored, err)
					}
					out, err := h.Run("axi", "status", "--run", run.ID)
					if err != nil {
						t.Fatal(err)
					}
					t.Logf("guard refused without publication or rewind:\n%s", out)
				}
				if log, err := os.ReadFile(filepath.Join(h.NMHome, "logs", run.ID, "push.log")); err == nil {
					t.Logf("product Push log:\n%s", log)
				}
				j.finish(branch)
			}
			data, err := os.ReadFile(trace)
			if err != nil || !strings.Contains(string(data), "INJECT "+kind) {
				t.Fatalf("race never reached the running Push step: %s %v", data, err)
			}
			t.Logf("real Git remote race trace:\n%s", data)
		})
	}
}

const pushTargetRacePython = `import json, os, sqlite3, subprocess, sys
git, home, trace, trigger, kind, upstream, target, state, branch = %q, %q, %q, %d, %q, %q, %q, %q, %q
dbpath = os.path.join(home, "state.sqlite")
if os.path.exists(dbpath):
    with sqlite3.connect("file:" + dbpath + "?mode=ro", uri=True) as db:
        pushing = db.execute("SELECT COUNT(*) FROM step_results WHERE step_name='push' AND status='running'").fetchone()[0]
    if pushing:
        lines = open(trace).readlines() if os.path.exists(trace) else []
        n = sum(line.startswith("fetch ") for line in lines) + 1
        with open(trace, "a") as log:
            log.write("fetch " + str(n) + " " + repr(sys.argv[1:]) + "\n")
            if n == trigger:
                log.write("INJECT " + kind + "\n")
                log.flush()
                if kind == "unreadable target":
                    os.rename(upstream, upstream + ".offline")
                elif kind in ("changed PR", "changed head"):
                    data = json.load(open(state))
                    if kind == "changed PR":
                        data["prs"][branch]["base"] = "develop"
                    else:
                        subprocess.check_call([git, "--git-dir=" + upstream, "update-ref", "refs/heads/" + branch, target])
                    with open(state, "w") as output:
                        json.dump(data, output)
                else:
                    subprocess.check_call([git, "--git-dir=" + upstream, "update-ref", "refs/heads/main", target])
os.execv(git, [git] + sys.argv[1:])
`
