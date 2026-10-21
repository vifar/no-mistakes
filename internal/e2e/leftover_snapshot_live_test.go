//go:build e2e && !windows

package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// The independent oracle is the recovery contract: Git must apply the saved
// patches after source-worktree removal, and the restored bytes must equal the
// deliberately supplied index/worktree versions. A plausible broken capture
// duplicates replacement patches, includes scratch, or follows display config.
func TestLeftoverSnapshotRecoveryLiveJourney(t *testing.T) {
	for _, kind := range []string{"mixed_bytes", "deleted_survivor", "replacement_staged", "replacement_intent", "large_scratch", "gitlink", "git_filename_bytes", "clean", "write_failure", "conflict"} {
		t.Run(kind, func(t *testing.T) {
			h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: cleanReviewScenario(t)})
			root := filepath.Dir(h.AgentLog)
			script := filepath.Join(root, "snapshot-edits.sh")
			marker := filepath.Join(root, "edits-done")
			config := fmt.Sprintf("commands:\n  test: %q\n  lint: 'true'\n", "sh "+shellQuote(script))
			if kind != "clean" && kind != "write_failure" && kind != "conflict" {
				config += "protected_paths: [feature.txt, module]\n"
			}
			commitTrustedRepoConfig(t, h, config)
			if kind == "gitlink" {
				old := h.UpstreamBranchSHA("main")
				gitIn(t, h, h.WorkDir, "update-index", "--add", "--cacheinfo", "160000,"+old+",module")
				gitIn(t, h, h.WorkDir, "commit", "-m", "seed gitlink")
				gitIn(t, h, h.WorkDir, "push", "origin", "main")
			}
			base := h.UpstreamBranchSHA("main")
			targetFile := "landed.txt"
			if kind == "conflict" {
				targetFile = "feature.txt"
			}
			target := h.CommitChange("main", targetFile, "advanced target\n", "prepare target advance")
			gitIn(t, h, h.WorkDir, "push", "origin", target+":refs/heads/prepared-target")
			gitIn(t, h, h.WorkDir, "reset", "--hard", base)
			const branch = "feature/snapshot-recovery"
			head := h.CommitChange(branch, "feature.txt", "submitted feature\n", "submit feature")
			edit := "printf 'working version\\n' > feature.txt\n"
			switch kind {
			case "mixed_bytes":
				edit = "printf 'index version\\n' > feature.txt\ngit add feature.txt\nprintf 'working version\\n' > feature.txt\nprintf '\\000\\377\\001' > 'new\nfile.bin'\nchmod 750 'new\nfile.bin'\nln -s \"$(printf 'target\\377')\" link-one\nln -s \"$(printf 'target\\376')\" link-two\ngit config color.ui always\ngit config color.diff always\ngit config diff.noprefix true\ngit config diff.outputIndicatorNew '^'\n"
			case "deleted_survivor":
				edit = "git rm --cached feature.txt\nprintf 'working version\\n' > feature.txt\n"
			case "replacement_staged", "replacement_intent":
				edit = "rm feature.txt\nmkdir -p feature.txt/.journal-live\nprintf 'working version\\n' > 'feature.txt/real[1].txt'\nprintf 'private scratch\\n' > feature.txt/.journal-live/postgres.log\n"
				if kind == "replacement_intent" {
					edit += "git add -N -A\n"
				} else {
					edit += "git add -A\n"
				}
			case "large_scratch":
				edit += "mkdir -p .journal-live\ni=0\nwhile [ $i -lt 12000 ]; do printf 'private scratch\\n' > .journal-live/$(printf '%05d' $i)-" + strings.Repeat("x", 200) + ".log; i=$((i+1)); done\n"
			case "gitlink":
				edit += fmt.Sprintf("git clone %s module\ngit -C module checkout --detach %s\ngit update-index --cacheinfo 160000,%s,module\ngit config diff.ignoreSubmodules all\ngit config diff.submodule log\n", shellQuote(h.WorkDir), head, head)
			case "git_filename_bytes":
				edit += "blob=$(printf 'index byte name\\n' | git hash-object -w --stdin)\ngit update-index --add --cacheinfo \"100644,$blob,$(printf 'x\\377')\"\ngit update-index --add --cacheinfo \"100644,$blob,$(printf 'x\\376')\"\n"
			case "clean":
				edit = "mkdir -p .journal-live\nprintf 'private scratch\\n' > .journal-live/postgres.log\n"
			case "write_failure":
				edit += "mkdir -p .journal-live\nprintf 'private scratch\\n' > .journal-live/postgres.log\ngit add .journal-live/postgres.log\nchmod 500 \"$NM_HOME\"/logs/*\n"
			}
			commands := "#!/bin/sh\nset -eu\nif [ -f " + shellQuote(marker) + " ]; then exit 0; fi\ntouch " + shellQuote(marker) + "\n" + edit
			commands += fmt.Sprintf("git --git-dir=%s update-ref refs/heads/main %s\n", shellQuote(h.UpstreamDir), target)
			if err := os.WriteFile(script, []byte(commands), 0700); err != nil {
				t.Fatal(err)
			}
			// Restore permissions before the harness removes its disposable root.
			t.Cleanup(func() {
				dirs, _ := filepath.Glob(filepath.Join(h.NMHome, "logs", "*"))
				for _, dir := range dirs {
					_ = os.Chmod(dir, 0700)
				}
			})
			initFromOwnWorktree(t, h, "init-snapshot-recovery")
			h.Checkout(branch)
			h.PushToGate(branch)
			var result *ipc.RunInfo
			if kind != "clean" {
				result = waitForStepStatus(t, h, branch, types.StepRebase, types.StepStatusAwaitingApproval, 90*time.Second)
			} else {
				result = h.WaitForRun(branch, 90*time.Second)
			}
			out, err := h.Run("axi", "status", "--run", result.ID)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("real product status:\n%s", out)
			worktree := paths.WithRoot(h.NMHome).WorktreeDir(result.RepoID, result.ID)
			files, err := filepath.Glob(filepath.Join(h.NMHome, "logs", result.ID, "leftover-validation-*.json"))
			if err != nil {
				t.Fatal(err)
			}
			if kind == "clean" {
				if result.Status != types.RunCompleted || len(files) != 0 {
					t.Fatalf("scratch-only rewind: status=%s snapshots=%v", result.Status, files)
				}
				t.Log("scratch-only rewind completed without creating a recovery snapshot")
				return
			}
			if kind == "write_failure" {
				refused := false
				for _, step := range result.Steps {
					if step.StepName == types.StepRebase && step.Status == types.StepStatusAwaitingApproval && step.FindingsJSON != nil {
						refused = pipeline.HasProtectedPathRefusal(*step.FindingsJSON) && strings.Contains(*step.FindingsJSON, "save leftover validation evidence") && strings.Contains(*step.FindingsJSON, "test step left uncommitted edits")
					}
				}
				if !refused || len(files) != 0 {
					t.Fatalf("snapshot write failure did not preserve the run at a refusal gate: %s %v", result.Status, files)
				}
				if _, err := os.Stat(worktree); err != nil {
					t.Fatalf("snapshot save refusal lost the unsaved worktree: no recovery snapshot exists and worktree inspection failed: %v", err)
				}
				if got := gitIn(t, h, worktree, "rev-parse", "HEAD"); got != head {
					t.Fatal("snapshot failure moved head")
				}
				if got := gitIn(t, h, worktree, "show", ":.journal-live/postgres.log"); got != "private scratch" {
					t.Fatal("snapshot failure unstaged scratch")
				}
				got, err := os.ReadFile(filepath.Join(worktree, "feature.txt"))
				if err != nil || string(got) != "working version\n" {
					t.Fatalf("snapshot failure altered edits: %q %v", got, err)
				}
				t.Log("snapshot write refusal preserved entry HEAD, working bytes, and staged scratch")
				return
			}
			if kind != "conflict" {
				attributed := false
				for _, step := range result.Steps {
					if step.StepName == types.StepRebase && step.FindingsJSON != nil {
						attributed = strings.Contains(*step.FindingsJSON, "test step left uncommitted edits") && strings.Contains(*step.FindingsJSON, "protected-path-refusal")
					}
				}
				if !attributed {
					t.Fatalf("missing attributed protected-path refusal: %s", out)
				}
			}
			if len(files) != 1 {
				t.Fatalf("expected one durable snapshot: %v", files)
			}
			data, err := os.ReadFile(files[0])
			if err != nil {
				t.Fatal(err)
			}
			var record struct {
				Producer      string   `json:"producer"`
				Head          string   `json:"head"`
				Paths         [][]byte `json:"paths"`
				Status        []byte   `json:"status"`
				StagedPatch   []byte   `json:"staged_patch"`
				UnstagedPatch []byte   `json:"unstaged_patch"`
				Untracked     []struct {
					Path          []byte `json:"path"`
					Data          []byte `json:"data"`
					SymlinkTarget []byte `json:"symlink_target"`
					Mode          uint32 `json:"mode"`
				} `json:"untracked"`
			}
			if err := json.Unmarshal(data, &record); err != nil {
				t.Fatal(err)
			}
			if record.Producer != "test" || record.Head != head || len(record.Status) == 0 || record.Status[len(record.Status)-1] != 0 {
				t.Fatalf("snapshot lost producer, entry head or NUL status: %+v", record)
			}
			for _, stream := range [][]byte{record.Status, record.StagedPatch, record.UnstagedPatch} {
				if bytes.Contains(stream, []byte(".journal-live")) || bytes.Contains(stream, []byte("private scratch")) || bytes.Contains(stream, []byte{27}) {
					t.Fatal("snapshot contains scratch or ANSI display bytes")
				}
			}
			info, err := os.Stat(files[0])
			if err != nil || info.Mode().Perm() != 0600 {
				t.Fatalf("snapshot is not private: %v %v", info, err)
			}
			if err := os.RemoveAll(worktree); err != nil {
				t.Fatal(err)
			}
			recovery := filepath.Join(root, "recovery")
			gitIn(t, h, root, "clone", "--no-checkout", h.WorkDir, recovery)
			gitIn(t, h, recovery, "checkout", "--detach", record.Head)
			for i, patch := range [][]byte{record.StagedPatch, record.UnstagedPatch} {
				if len(patch) == 0 {
					continue
				}
				file := filepath.Join(root, fmt.Sprintf("recovery-%d.patch", i))
				if err := os.WriteFile(file, patch, 0600); err != nil {
					t.Fatal(err)
				}
				args := []string{"apply"}
				if i == 0 {
					args = append(args, "--index")
				}
				if kind == "git_filename_bytes" {
					args = []string{"apply", "--cached"}
				}
				gitIn(t, h, recovery, append(args, file)...)
				if kind == "mixed_bytes" && i == 0 && gitIn(t, h, recovery, "show", ":feature.txt") != "index version" {
					t.Fatal("staged version did not recover")
				}
				if kind == "git_filename_bytes" && i == 0 {
					for _, name := range []string{"x\xff", "x\xfe"} {
						if got := gitIn(t, h, recovery, "show", ":"+name); got != "index byte name" {
							t.Fatalf("lost byte filename %q: %q", name, got)
						}
					}
				}
			}
			for _, file := range record.Untracked {
				path := filepath.Join(recovery, string(file.Path))
				if file.SymlinkTarget != nil {
					err = os.Symlink(string(file.SymlinkTarget), path)
				} else {
					err = os.WriteFile(path, file.Data, 0600)
					if err == nil {
						err = os.Chmod(path, os.FileMode(file.Mode))
					}
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			path := "feature.txt"
			if strings.HasPrefix(kind, "replacement_") {
				path += "/real[1].txt"
			}
			if kind != "git_filename_bytes" {
				got, err := os.ReadFile(filepath.Join(recovery, path))
				if err != nil || string(got) != "working version\n" {
					t.Fatalf("working bytes not recovered: %q %v", got, err)
				}
			}
			if kind == "mixed_bytes" {
				got, err := os.ReadFile(filepath.Join(recovery, "new\nfile.bin"))
				if err != nil || !bytes.Equal(got, []byte{0, 255, 1}) {
					t.Fatalf("binary bytes lost: %x %v", got, err)
				}
				info, err := os.Stat(filepath.Join(recovery, "new\nfile.bin"))
				if err != nil || info.Mode().Perm() != 0750 {
					t.Fatalf("untracked mode lost: %v %v", info, err)
				}
				for name, target := range map[string]string{"link-one": "target\xff", "link-two": "target\xfe"} {
					if got, err := os.Readlink(filepath.Join(recovery, name)); err != nil || got != target {
						t.Fatalf("symlink target bytes lost: %q %v", got, err)
					}
				}
			}
			if kind == "deleted_survivor" {
				for _, status := range []string{"D  feature.txt\x00", "?? feature.txt\x00"} {
					if bytes.Count(record.Status, []byte(status)) != 1 {
						t.Fatalf("lost duplicate-path status: %q", record.Status)
					}
				}
			}
			if kind == "gitlink" && gitIn(t, h, recovery, "rev-parse", ":module") != head {
				t.Fatal("gitlink not recovered under inherited diff settings")
			}
			if got, err := os.ReadFile(files[0]); err != nil || !bytes.Equal(got, data) {
				t.Fatalf("cleanup changed snapshot: %v", err)
			}
			t.Logf("source worktree removed; private snapshot from %s at %s recovered %q using real git apply", record.Producer, record.Head, record.Paths)
			if evidence := os.Getenv("NM_SNAPSHOT_TEST_EVIDENCE"); evidence != "" {
				if err := os.WriteFile(filepath.Join(evidence, kind+"-snapshot.json"), data, 0600); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}
