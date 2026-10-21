package steps

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
)

// Status alone cannot admit these entries: assume-unchanged and skip-worktree
// hide raw bytes that whole-tree rollback would otherwise overwrite.
func TestDocumentHiddenIndexEntryRefusesBeforeAgentOrEarlyReturn(t *testing.T) {
	for _, flag := range []string{"--assume-unchanged", "--skip-worktree"} {
		for _, scenario := range []string{"repeat", "cached completion", "zero budget", "ignored comparison", "unchanged head", "flag without edit"} {
			t.Run(flag+"/"+scenario, func(t *testing.T) {
				dir, base, head := setupGitRepo(t)
				calls := 0
				ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
					calls++
					if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("repair\n"), 0o644); err != nil {
						t.Fatal(err)
					}
					return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"repair README"}`)}, nil
				}}
				sc := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{Lint: "true"})
				path := "feature.txt"
				switch scenario {
				case "repeat", "cached completion":
					if _, err := (&DocumentStep{}).Execute(sc); err != nil {
						t.Fatal(err)
					}
					if scenario == "repeat" {
						gitCmd(t, dir, "commit", "--allow-empty", "-qm", "advance without changing input tree")
						sc.Run.HeadSHA = gitCmd(t, dir, "rev-parse", "HEAD")
						if err := sc.DB.UpdateRunHeadSHA(sc.Run.ID, sc.Run.HeadSHA); err != nil {
							t.Fatal(err)
						}
						// The next repair differs from the accepted README, but not
						// in filename. The old rollback also touches feature.txt.
						ag.runFn = func(context.Context, agent.RunOpts) (*agent.Result, error) {
							calls++
							if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("second repair\n"), 0o644); err != nil {
								t.Fatal(err)
							}
							return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"second repair"}`)}, nil
						}
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
					path = "base.txt"
				}
				before, err := os.ReadFile(filepath.Join(dir, path))
				if err != nil {
					t.Fatal(err)
				}
				gitCmd(t, dir, "update-index", flag, "--", path)
				if scenario != "flag without edit" {
					before = []byte("pre-existing hidden owner edit\n")
					if err := os.WriteFile(filepath.Join(dir, path), before, 0o644); err != nil {
						t.Fatal(err)
					}
				}
				if status := gitStatusPorcelain(t, dir); status != "" {
					t.Fatalf("fixture did not hide the edit: %s", status)
				}
				flagsBefore := gitCmd(t, dir, "ls-files", "-v")
				headBefore := gitCmd(t, dir, "rev-parse", "HEAD")
				passesBefore, err := sc.DB.GetDocumentPasses(sc.Run.ID)
				if err != nil {
					t.Fatal(err)
				}
				callsBefore := calls
				out, err := (&DocumentStep{}).Execute(sc)
				if err == nil || !strings.Contains(err.Error(), "worktree has 1 entries hidden from status by index flags") || !strings.Contains(err.Error(), path) || out != nil {
					t.Errorf("hidden entry lacked named refusal: outcome=%+v error=%v", out, err)
				}
				if calls != callsBefore || gitCmd(t, dir, "rev-parse", "HEAD") != headBefore {
					t.Error("hidden entry launched an agent or changed HEAD")
				}
				if got, err := os.ReadFile(filepath.Join(dir, path)); err != nil || string(got) != string(before) {
					t.Errorf("pre-existing hidden bytes lost from %s: got=%q want=%q error=%v", path, got, before, err)
				}
				if got := gitCmd(t, dir, "ls-files", "-v"); got != flagsBefore {
					t.Errorf("admission changed index flags: %q != %q", got, flagsBefore)
				}
				passes, err := sc.DB.GetDocumentPasses(sc.Run.ID)
				if err != nil || len(passes) != len(passesBefore) {
					t.Errorf("refusal recorded a new Document pass: %+v, %v", passes, err)
				}
			})
		}
	}
}

// Admission is not enough: an agent can set an index flag after it starts.
// Refuse rollback before touching either the hidden bytes or its README repair.
func TestDocumentHiddenIndexIntroducedDuringProposalRefusesRollback(t *testing.T) {
	for _, flag := range []string{"--assume-unchanged", "--skip-worktree"} {
		t.Run(flag, func(t *testing.T) {
			dir, base, head := setupGitRepo(t)
			ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
				gitCmd(t, dir, "update-index", flag, "--", "feature.txt")
				if err := os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("hidden during proposal\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				// Exceed the commit budget so rollback is required even though
				// this is the run's first Document pass.
				writeAndCommit("README.md", "first proposal\n")(t, dir)
				writeAndCommit("guide.md", "second proposal\n")(t, dir)
				return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"update guides"}`)}, nil
			}}
			sc := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{Lint: "true"})
			sc.Config.Document.MaxCommitsPerRun = 1
			out, err := (&DocumentStep{}).Execute(sc)
			if err == nil || !strings.Contains(err.Error(), "worktree has 1 entries hidden from status by index flags") || !strings.Contains(err.Error(), "feature.txt") || out != nil {
				t.Errorf("unsafe rollback was not refused: outcome=%+v error=%v", out, err)
			}
			if got, err := os.ReadFile(filepath.Join(dir, "feature.txt")); err != nil || string(got) != "hidden during proposal\n" {
				t.Errorf("rollback destroyed hidden bytes: %q, %v", got, err)
			}
			if got := gitCmd(t, dir, "rev-list", "--count", head+"..HEAD"); got != "2" {
				t.Errorf("refused rollback moved HEAD: %s commits remain", got)
			}
			for path, want := range map[string]string{"README.md": "first proposal\n", "guide.md": "second proposal\n"} {
				if got, err := os.ReadFile(filepath.Join(dir, path)); err != nil || string(got) != want {
					t.Errorf("refused rollback changed proposal %s: %q, %v", path, got, err)
				}
			}
			passes, err := sc.DB.GetDocumentPasses(sc.Run.ID)
			if err != nil || len(passes) != 1 || passes[0].ToHeadSHA != nil {
				t.Errorf("unsafe rollback was marked completed: %+v, %v", passes, err)
			}
		})
	}
}

// Issue #90's oracle is raw byte preservation, not porcelain status. The
// admission check ran before the agent; accepting this pass would certify
// bytes that neither the proposal enumeration nor the input tree can see.
func TestDocumentHiddenIndexIntroducedDuringAcceptedPass(t *testing.T) {
	for _, flag := range []string{"--assume-unchanged", "--skip-worktree"} {
		for _, edit := range []bool{false, true} {
			t.Run(flag+"/edit="+fmt.Sprint(edit), func(t *testing.T) {
				dir, base, head := setupGitRepo(t)
				want, err := os.ReadFile(filepath.Join(dir, "feature.txt"))
				if err != nil {
					t.Fatal(err)
				}
				ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
					gitCmd(t, dir, "update-index", flag, "--", "feature.txt")
					if edit {
						want = []byte("hidden owner bytes must survive\n")
						if err := os.WriteFile(filepath.Join(dir, "feature.txt"), want, 0o644); err != nil {
							t.Fatal(err)
						}
					}
					return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"no remaining gap"}`)}, nil
				}}
				sc := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{Lint: "true"})
				out, err := (&DocumentStep{}).Execute(sc)
				if err == nil || out != nil || !strings.Contains(err.Error(), "worktree has 1 entries hidden from status by index flags; Document cannot verify a clean entry") {
					t.Errorf("accepted an unverifiable pass: outcome=%+v error=%v", out, err)
				}
				if got, err := os.ReadFile(filepath.Join(dir, "feature.txt")); err != nil || !bytes.Equal(got, want) {
					t.Fatalf("hidden bytes lost: got=%q want=%q error=%v", got, want, err)
				}
				if got := gitCmd(t, dir, "rev-parse", "HEAD"); got != head {
					t.Fatalf("refusal changed HEAD: %s", got)
				}
				passes, err := sc.DB.GetDocumentPasses(sc.Run.ID)
				if err != nil || len(passes) != 1 || passes[0].ToHeadSHA != nil {
					t.Fatalf("unverifiable pass marked complete: %+v error=%v", passes, err)
				}
			})
		}
	}
}

func TestDocumentHiddenIndexCannotProveRepeatedInput(t *testing.T) {
	dir, base, head := setupGitRepo(t)
	ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		writeAndCommit("README.md", "accepted guide\n")(t, dir)
		return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"guide repaired"}`)}, nil
	}}
	sc := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{Lint: "true"})
	if _, err := (&DocumentStep{}).Execute(sc); err != nil {
		t.Fatal(err)
	}
	passes, err := sc.DB.GetDocumentPasses(sc.Run.ID)
	if err != nil || len(passes) != 1 {
		t.Fatalf("accepted pass: %+v %v", passes, err)
	}
	gitCmd(t, dir, "update-index", "--assume-unchanged", "feature.txt")
	if err := os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("different raw input\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	repeated, err := documentRepeatsUnchangedInputs(sc, &passes[0], sc.Run.HeadSHA, []string{"README.md"})
	if err == nil || repeated {
		t.Fatalf("hidden input certified as repeated: repeated=%v error=%v", repeated, err)
	}
}

// This directly exercises the rollback boundary so a future admission check
// cannot mask a destructive reset. Replacing rollback with the #69 version
// really destroys feature.txt and this test reports the lost bytes.
func TestDocumentRollbackPreservesHiddenOwnerBytes(t *testing.T) {
	dir, base, head := setupGitRepo(t)
	sc := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, base, head, config.Commands{Lint: "true"})
	gitCmd(t, dir, "update-index", "--assume-unchanged", "feature.txt")
	const want = "pre-existing hidden owner bytes\n"
	if err := os.WriteFile(filepath.Join(dir, "feature.txt"), []byte(want), 0o644); err != nil {
		t.Fatal(err)
	}
	writeAndCommit("README.md", "rejected proposal\n")(t, dir)
	proposalHead := gitCmd(t, dir, "rev-parse", "HEAD")
	err := discardDocumentCandidate(sc, head, []string{"README.md"}, nil, nil)
	if got, readErr := os.ReadFile(filepath.Join(dir, "feature.txt")); readErr != nil || string(got) != want {
		t.Errorf("rollback destroyed hidden owner edit: got=%q want=%q error=%v", got, want, readErr)
	}
	if err == nil {
		t.Error("rollback must refuse hidden index flags")
	}
	if got := gitCmd(t, dir, "rev-parse", "HEAD"); got != proposalHead {
		t.Errorf("unsafe rollback moved head: %s", got)
	}
}

func TestDocumentHiddenIndexAdmissionCountsEveryFlaggedEntry(t *testing.T) {
	dir, base, head := setupGitRepo(t)
	// Include a newline to prove enumeration counts NUL-delimited index entries,
	// rather than lines in status output. Both flags together produce 's'.
	path := "guide\nwith newline.md"
	writeAndCommit(path, "unchanged bytes\n")(t, dir)
	head = gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "update-index", "--assume-unchanged", "--", path)
	gitCmd(t, dir, "update-index", "--skip-worktree", "--", path)
	gitCmd(t, dir, "update-index", "--assume-unchanged", "--", "feature.txt")
	sc := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, base, head, config.Commands{Lint: "true"})
	out, err := (&DocumentStep{}).Execute(sc)
	if out != nil || err == nil || !strings.Contains(err.Error(), "worktree has 2 entries hidden from status by index flags; Document cannot verify a clean entry") {
		t.Fatalf("wrong hidden entry count: outcome=%+v error=%v", out, err)
	}
}
