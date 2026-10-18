package steps

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
)

// A different commit with identical input content is still a repeat. Discarding
// it must park rather than report the reverted README repair as clean.
func TestDocumentRepeatedCleanRepairParksAndRetainsFinding(t *testing.T) {
	for _, test := range []struct {
		selfCommit bool
		mergeHead  bool
	}{{false, false}, {true, false}, {false, true}, {true, true}} {
		t.Run(fmt.Sprintf("self_commit=%t/merge_head=%t", test.selfCommit, test.mergeHead), func(t *testing.T) {
			dir, base, head := setupGitRepo(t)
			calls := 0
			ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
				calls++
				if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte(fmt.Sprintf("guide %d\n", calls)), 0o644); err != nil {
					t.Fatal(err)
				}
				if test.selfCommit {
					gitCmd(t, dir, "add", "README.md")
					gitCmd(t, dir, "commit", "-qm", "agent README repair")
				}
				return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"repair README"}`)}, nil
			}}
			sc := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{Lint: "true"})
			if out, err := (&DocumentStep{}).Execute(sc); err != nil || out.NeedsApproval {
				t.Fatalf("initial repair: %+v, %v", out, err)
			}
			acceptedTree := gitCmd(t, dir, "rev-parse", "HEAD^{tree}")
			if test.mergeHead {
				// Diverge two empty commits, then make a real two-parent merge.
				// The head identity changes while the accepted input tree does not.
				side := gitCmd(t, dir, "commit-tree", acceptedTree, "-p", "HEAD", "-m", "identical-tree side")
				gitCmd(t, dir, "commit", "--allow-empty", "-qm", "identical-tree main")
				gitCmd(t, dir, "merge", "--no-ff", "-m", "merge identical input trees", side)
				if parents := strings.Fields(gitCmd(t, dir, "show", "-s", "--format=%P", "HEAD")); len(parents) != 2 {
					t.Fatalf("fixture is not a merge commit: %v", parents)
				}
			} else {
				gitCmd(t, dir, "commit", "--allow-empty", "-qm", "advance head with identical inputs")
			}
			if got := gitCmd(t, dir, "rev-parse", "HEAD^{tree}"); got != acceptedTree {
				t.Fatalf("head advance changed inputs: %s != %s", got, acceptedTree)
			}
			accepted := gitCmd(t, dir, "rev-parse", "HEAD")
			sc.Run.HeadSHA = accepted
			if err := sc.DB.UpdateRunHeadSHA(sc.Run.ID, accepted); err != nil {
				t.Fatal(err)
			}
			for attempt := 0; attempt < 2; attempt++ {
				out, err := (&DocumentStep{}).Execute(sc)
				if err != nil {
					t.Fatal(err)
				}
				if got := gitCmd(t, dir, "rev-parse", "HEAD"); got != accepted {
					t.Fatalf("discarded repeat advanced HEAD: %s, want %s", got, accepted)
				}
				if got := gitCmd(t, dir, "show", "HEAD:README.md"); got != "guide 1" {
					t.Fatalf("discarded repair changed accepted README: %q", got)
				}
				if got := gitStatusPorcelain(t, dir); got != "" {
					t.Fatalf("discarded repair left worktree edits: %s", got)
				}
				if !out.NeedsApproval {
					t.Fatalf("discarded clean README repair manufactured success: %+v", out)
				}
				var findings Findings
				if err := json.Unmarshal([]byte(out.Findings), &findings); err != nil {
					t.Fatal(err)
				}
				if len(findings.Items) != 1 || findings.Items[0].Action != "ask-user" || findings.Items[0].File != "README.md" {
					t.Fatalf("missing README owner decision: %s", out.Findings)
				}
				if !strings.Contains(strings.ToLower(findings.Items[0].Description), "discard") {
					t.Fatalf("finding does not explain the unapplied repair: %s", out.Findings)
				}
				passes, err := sc.DB.GetDocumentPasses(sc.Run.ID)
				if err != nil || len(passes) != attempt+2 {
					t.Fatalf("pass records: %+v, %v", passes, err)
				}
				last := passes[len(passes)-1]
				if last.CommitCount != 0 || last.ToHeadSHA == nil || *last.ToHeadSHA != accepted || last.FindingsJSON == nil {
					t.Fatalf("repeat was not durably stopped at its entry head: %+v", last)
				}
				var persisted Findings
				if err := json.Unmarshal([]byte(*last.FindingsJSON), &persisted); err != nil || len(persisted.Items) != 1 || persisted.Items[0].Description != findings.Items[0].Description {
					t.Fatalf("pending finding lost on later attempt: %s, %v", *last.FindingsJSON, err)
				}
				fresh := *sc
				fresh.Shared = &pipeline.RunShared{}
				sc = &fresh
			}
			if calls != 3 {
				t.Fatalf("expected both suppressed proposals to run, got %d calls", calls)
			}
		})
	}
}

func TestDocumentForeignEntryRefusesBeforeConvergenceEarlyReturns(t *testing.T) {
	for _, scenario := range []string{"previous clean pass", "zero budget", "ignored comparison", "unchanged head"} {
		t.Run(scenario, func(t *testing.T) {
			dir, base, head := setupGitRepo(t)
			ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
				if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("guide\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"guide"}`)}, nil
			}}
			sc := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{Lint: "true"})
			switch scenario {
			case "previous clean pass":
				if _, err := (&DocumentStep{}).Execute(sc); err != nil {
					t.Fatal(err)
				}
			case "zero budget":
				sc.Config.Document.MaxCommitsPerRun = 0
			case "ignored comparison":
				sc.Config.IgnorePatterns = []string{"**"}
			case "unchanged head":
				gitCmd(t, dir, "checkout", "--detach", base)
				sc.Run.HeadSHA = base
				if err := sc.DB.UpdateRunHeadSHA(sc.Run.ID, base); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(filepath.Join(dir, "foreign-owner.md"), []byte("owner work\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			calls := len(ag.calls)
			headBefore := gitCmd(t, dir, "rev-parse", "HEAD")
			if out, err := (&DocumentStep{}).Execute(sc); err == nil {
				t.Fatalf("%s bypassed foreign-entry admission: %+v", scenario, out)
			}
			if len(ag.calls) != calls || gitCmd(t, dir, "rev-parse", "HEAD") != headBefore {
				t.Fatal("foreign admission refusal launched an agent or changed HEAD")
			}
			if b, err := os.ReadFile(filepath.Join(dir, "foreign-owner.md")); err != nil || string(b) != "owner work\n" {
				t.Fatalf("foreign input was lost: %q, %v", b, err)
			}
		})
	}
}

// Discarding the combined pass also discards its formatter repair. Lint must
// reassess the restored bytes, never consume a clean result from that proposal.
func TestDocumentDiscardedSameFileRepairKeepsLintDuty(t *testing.T) {
	dir, base, head := setupGitRepo(t)
	housekeepingCalls, lintCalls := 0, 0
	ag := &mockAgent{name: "test", runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
		switch opts.Purpose {
		case "housekeeping":
			housekeepingCalls++
			content := "guide 1 with bad spacing\n"
			if housekeepingCalls > 1 {
				content = "guide 2 formatted\n"
			}
			if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
		case "lint":
			lintCalls++
			if got, err := os.ReadFile(filepath.Join(dir, "README.md")); err != nil || string(got) != "guide 1 with bad spacing\n" {
				t.Fatalf("lint did not reassess restored pre-pass bytes: %q, %v", got, err)
			}
			return &agent.Result{Output: json.RawMessage(`{"findings":[{"severity":"warning","file":"README.md","description":"formatter repair was discarded and still needs attention","action":"ask-user"}],"summary":"formatter repair still needed"}`)}, nil
		default:
			t.Fatalf("unexpected agent purpose %q", opts.Purpose)
		}
		return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"repair guide and formatting"}`)}, nil
	}}
	sc := newHousekeepingContext(t, ag, dir, base, head, config.Commands{})
	if out, err := (&DocumentStep{}).Execute(sc); err != nil || out.NeedsApproval {
		t.Fatalf("initial combined repair: %+v, %v", out, err)
	}
	gitCmd(t, dir, "commit", "--allow-empty", "-qm", "advance head with identical inputs")
	accepted := gitCmd(t, dir, "rev-parse", "HEAD")
	sc.Run.HeadSHA = accepted
	if err := sc.DB.UpdateRunHeadSHA(sc.Run.ID, accepted); err != nil {
		t.Fatal(err)
	}
	out, err := (&DocumentStep{}).Execute(sc)
	if err != nil {
		t.Fatal(err)
	}
	if !out.NeedsApproval {
		t.Errorf("discarded same-file combined repair manufactured clean approval: %+v", out)
	}
	if got := gitCmd(t, dir, "rev-parse", "HEAD"); got != accepted {
		t.Fatalf("discarded combined proposal advanced HEAD: %s", got)
	}
	if got := gitStatusPorcelain(t, dir); got != "" {
		t.Fatalf("discarded combined proposal left edits: %s", got)
	}
	lint, err := (&LintStep{}).Execute(sc)
	if err != nil {
		t.Fatal(err)
	}
	if housekeepingCalls != 2 || lintCalls != 1 || !lint.NeedsApproval || !strings.Contains(lint.Findings, "formatter repair was discarded") {
		t.Fatalf("discarded combined duty vanished: housekeeping=%d lint=%d outcome=%+v", housekeepingCalls, lintCalls, lint)
	}
}

// Reusing the filename is necessary here: a different documentation filename
// cannot detect the former filename-only repeat suppression after code changes.
func TestDocumentSameFileRepairSurvivesChangedInputContent(t *testing.T) {
	for _, selfCommit := range []bool{false, true} {
		t.Run(fmt.Sprint(selfCommit), func(t *testing.T) {
			dir, base, head := setupGitRepo(t)
			calls := 0
			ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
				calls++
				if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte(fmt.Sprintf("guide %d\n", calls)), 0o644); err != nil {
					t.Fatal(err)
				}
				if selfCommit {
					gitCmd(t, dir, "add", "README.md")
					gitCmd(t, dir, "commit", "-qm", "repair same README for changed inputs")
				}
				return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"repair README"}`)}, nil
			}}
			sc := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{Lint: "true"})
			if out, err := (&DocumentStep{}).Execute(sc); err != nil || out.NeedsApproval {
				t.Fatalf("initial README repair: %+v, %v", out, err)
			}
			firstTree := gitCmd(t, dir, "rev-parse", "HEAD^{tree}")
			writeAndCommit("feature.txt", "code now requires a second README repair\n")(t, dir)
			inputHead := gitCmd(t, dir, "rev-parse", "HEAD")
			if got := gitCmd(t, dir, "rev-parse", "HEAD^{tree}"); got == firstTree {
				t.Fatal("fixture did not change the documentation inputs")
			}
			sc.Run.HeadSHA = inputHead
			if err := sc.DB.UpdateRunHeadSHA(sc.Run.ID, inputHead); err != nil {
				t.Fatal(err)
			}
			out, err := (&DocumentStep{}).Execute(sc)
			if err != nil {
				t.Fatal(err)
			}
			if got := gitCmd(t, dir, "show", "HEAD:README.md"); got != "guide 2" {
				t.Fatalf("legitimate same-file repair after changed inputs was discarded: %q", got)
			}
			if out.NeedsApproval || calls != 2 {
				t.Fatalf("changed-input repair did not complete: calls=%d outcome=%+v", calls, out)
			}
			if got := gitCmd(t, dir, "rev-list", "--count", inputHead+"..HEAD"); got != "1" {
				t.Fatalf("same-file repair made %s commits, want one", got)
			}
			if got := gitCmd(t, dir, "show", "HEAD:feature.txt"); got != "code now requires a second README repair" {
				t.Fatalf("repair lost the intervening input change: %q", got)
			}
			passes, err := sc.DB.GetDocumentPasses(sc.Run.ID)
			if err != nil || len(passes) != 2 || passes[1].CommitCount != 1 || passes[1].ToHeadSHA == nil || *passes[1].ToHeadSHA != sc.Run.HeadSHA {
				t.Fatalf("accepted same-file repair not persisted: %+v, %v", passes, err)
			}
		})
	}
}
