package steps

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
)

func TestDocumentRefusesHiddenUntrackedEntry(t *testing.T) {
	dir, base, head := setupGitRepo(t)
	gitCmd(t, dir, "config", "status.showUntrackedFiles", "no")
	ownerFile := filepath.Join(dir, "owner.md")
	const content = "owner work must survive\n"
	if err := os.WriteFile(ownerFile, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	calls := 0
	ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		calls++
		for _, p := range []string{"first.md", "second.md"} {
			if err := os.WriteFile(filepath.Join(dir, p), []byte("proposal\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			gitCmd(t, dir, "add", p)
			gitCmd(t, dir, "commit", "-qm", "proposal")
		}
		return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"proposal"}`)}, nil
	}}
	sc := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{Lint: "true"})
	sc.Config.Document.MaxCommitsPerRun = 1
	_, err := (&DocumentStep{}).Execute(sc)
	if err == nil || !strings.Contains(err.Error(), "clean worktree") || calls != 0 {
		t.Errorf("hidden entry launched agent: calls=%d err=%v", calls, err)
	}
	got, readErr := os.ReadFile(ownerFile)
	if readErr != nil || string(got) != content {
		t.Errorf("owner work lost: %q %v", got, readErr)
	}
	if got := gitCmd(t, dir, "rev-parse", "HEAD"); got != head {
		t.Errorf("HEAD advanced: %s", got)
	}
}

func TestDocumentCountsHiddenUntrackedProposal(t *testing.T) {
	dir, base, head := setupGitRepo(t)
	gitCmd(t, dir, "config", "status.showUntrackedFiles", "no")
	ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		writeAndCommit("first.md", "proposal\n")(t, dir)
		if err := os.WriteFile(filepath.Join(dir, "second.md"), []byte("proposal\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"proposal"}`)}, nil
	}}
	sc := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{Lint: "true"})
	sc.Config.Document.MaxCommitsPerRun = 1
	out, err := (&DocumentStep{}).Execute(sc)
	if err != nil || !out.NeedsApproval {
		t.Fatalf("over-budget result: %+v %v", out, err)
	}
	if got := gitCmd(t, dir, "rev-parse", "HEAD"); got != head {
		t.Errorf("over-budget proposal advanced HEAD: %s", got)
	}
	if got := gitCmd(t, dir, "status", "--porcelain", "--untracked-files=all", "--ignore-submodules=none"); got != "" {
		t.Errorf("proposal remains: %s", got)
	}
}

func TestDocumentSubmoduleStatusVisibility(t *testing.T) {
	for _, setting := range []string{"diff.ignoreSubmodules", "submodule.dependency.ignore"} {
		for _, state := range []string{"tracked", "untracked", "head"} {
			t.Run(setting+"/"+state, func(t *testing.T) {
				source, childBase, _ := setupGitRepo(t)
				dir, base, _ := setupGitRepo(t)
				gitCmd(t, dir, "-c", "protocol.file.allow=always", "submodule", "add", source, "dependency")
				gitCmd(t, dir, "commit", "-qam", "add dependency")
				head := gitCmd(t, dir, "rev-parse", "HEAD")
				gitCmd(t, dir, "config", setting, "all")
				child := filepath.Join(dir, "dependency")
				p := "feature.txt"
				if state == "untracked" {
					p = "owner.md"
				}
				if state == "head" {
					gitCmd(t, child, "checkout", "--detach", childBase)
				} else if err := os.WriteFile(filepath.Join(child, p), []byte("owner work\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				calls := 0
				ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
					calls++
					return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":""}`)}, nil
				}}
				sc := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{Lint: "true"})
				_, err := (&DocumentStep{}).Execute(sc)
				if err == nil || !strings.Contains(err.Error(), "clean worktree") || calls != 0 {
					t.Errorf("hidden submodule launched agent: calls=%d err=%v", calls, err)
				}
				if got := gitCmd(t, dir, "rev-parse", "HEAD"); got != head {
					t.Errorf("HEAD advanced: %s", got)
				}
				if state != "head" {
					got, err := os.ReadFile(filepath.Join(child, p))
					if err != nil || string(got) != "owner work\n" {
						t.Errorf("submodule work lost: %q %v", got, err)
					}
					if err := discardDocumentCandidate(sc, head, nil, nil); err == nil || !strings.Contains(err.Error(), "remain") {
						t.Errorf("rollback certified dirty submodule: %v", err)
					}
				}
			})
		}
	}
}

func TestDocumentRollbackPreservesRecursiveSubmoduleWork(t *testing.T) {
	for _, reason := range []string{"over-budget", "repeated"} {
		for _, state := range []string{"dirty", "clean"} {
			t.Run(reason+"/"+state, func(t *testing.T) {
				source, childBase, _ := setupGitRepo(t)
				dir, base, _ := setupGitRepo(t)
				gitCmd(t, dir, "-c", "protocol.file.allow=always", "submodule", "add", source, "dependency")
				gitCmd(t, dir, "commit", "-qam", "add dependency")
				head := gitCmd(t, dir, "rev-parse", "HEAD")
				gitCmd(t, dir, "config", "submodule.recurse", "true")
				child := filepath.Join(dir, "dependency")
				calls := 0
				var childHead, childIndex, childDiff string
				ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
					calls++
					if reason == "repeated" && calls == 1 {
						gitCmd(t, child, "checkout", "--detach", childBase)
						if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("accepted guide\n"), 0o644); err != nil {
							t.Fatal(err)
						}
						gitCmd(t, dir, "add", "README.md", "dependency")
						gitCmd(t, dir, "commit", "-qm", "update guide and dependency")
						return &agent.Result{Output: json.RawMessage(`{"findings":[{"severity":"warning","description":"gap remains","action":"ask-user"}],"summary":"update guide and dependency"}`)}, nil
					}
					if state == "dirty" {
						if err := os.WriteFile(filepath.Join(child, "base.txt"), []byte("staged submodule work\n"), 0o644); err != nil {
							t.Fatal(err)
						}
						gitCmd(t, child, "add", "base.txt")
						if err := os.WriteFile(filepath.Join(child, "base.txt"), []byte("unstaged submodule work\n"), 0o644); err != nil {
							t.Fatal(err)
						}
					}
					childHead = gitCmd(t, child, "rev-parse", "HEAD")
					childIndex = gitCmd(t, child, "write-tree")
					childDiff = gitCmd(t, child, "diff", "--binary")
					if reason == "over-budget" {
						writeAndCommit("guide-a.md", "proposal A\n")(t, dir)
						writeAndCommit("guide-b.md", "proposal B\n")(t, dir)
					} else {
						writeAndCommit("README.md", "repeated guide\n")(t, dir)
					}
					return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"proposed repair"}`)}, nil
				}}
				sc := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{Lint: "true"})
				sc.Config.Document.MaxCommitsPerRun = 1
				priorPasses := 0
				if reason == "repeated" {
					sc.Config.Document.MaxCommitsPerRun = 3
					if out, err := (&DocumentStep{}).Execute(sc); err != nil || !out.NeedsApproval {
						t.Fatalf("initial accepted repair: %+v, %v", out, err)
					}
					priorPasses = 1
				}
				accepted := sc.Run.HeadSHA
				out, err := (&DocumentStep{}).Execute(sc)
				if state == "dirty" {
					if err == nil || !strings.Contains(err.Error(), "remain in the worktree") {
						t.Errorf("rollback failed to refuse dirty submodule: outcome=%+v err=%v", out, err)
					}
					if got, readErr := os.ReadFile(filepath.Join(child, "base.txt")); readErr != nil || string(got) != "unstaged submodule work\n" {
						t.Errorf("rollback lost submodule worktree bytes: %q, %v", got, readErr)
					}
				} else if err != nil || !out.NeedsApproval {
					t.Errorf("clean rollback failed: %+v, %v", out, err)
				}
				if got := gitCmd(t, child, "rev-parse", "HEAD"); got != childHead {
					t.Errorf("rollback changed submodule HEAD: %s != %s", got, childHead)
				}
				if got := gitCmd(t, child, "write-tree"); got != childIndex {
					t.Errorf("rollback changed submodule index: %s != %s", got, childIndex)
				}
				if got := gitCmd(t, child, "diff", "--binary"); got != childDiff {
					t.Errorf("rollback changed submodule unstaged diff: %q != %q", got, childDiff)
				}
				if got := gitCmd(t, dir, "rev-parse", "HEAD"); got != accepted {
					t.Errorf("rollback did not restore accepted parent HEAD: %s != %s", got, accepted)
				}
				passes, readErr := sc.DB.GetDocumentPasses(sc.Run.ID)
				if readErr != nil || len(passes) != priorPasses+1 {
					t.Fatalf("pass records: %+v, %v", passes, readErr)
				}
				last := passes[len(passes)-1]
				if state == "dirty" {
					if last.ToHeadSHA != nil || last.FindingsJSON != nil || last.CommitCount != 0 {
						t.Errorf("dirty rollback completed its interrupted pass: %+v", last)
					}
				} else if last.ToHeadSHA == nil || *last.ToHeadSHA != accepted || last.CommitCount != 0 || gitStatusPorcelain(t, dir) != "" {
					t.Errorf("clean rollback did not complete at clean accepted head: %+v", last)
				}
			})
		}
	}
}
