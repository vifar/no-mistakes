package steps

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Recovery must retain the index and working copies that Git actually reports,
// even when one path has two status records or the operator forces diff color.
func TestTargetRewindSnapshotRecoveryVariants(t *testing.T) {
	for _, kind := range []string{"deleted index with surviving file", "forced color", "custom diff display"} {
		t.Run(kind, func(t *testing.T) {
			c, _ := rewindFixture(t)
			c.RunLogDir = t.TempDir()
			head := c.Run.HeadSHA
			staged, working := []byte("index version\n"), []byte("surviving working version\n")
			path := filepath.Join(c.WorkDir, "feature.txt")
			if kind != "deleted index with surviving file" {
				gitCmd(t, c.WorkDir, "config", "color.ui", "always")
				gitCmd(t, c.WorkDir, "config", "color.diff", "always")
				if kind == "custom diff display" {
					gitCmd(t, c.WorkDir, "config", "diff.noprefix", "true")
					gitCmd(t, c.WorkDir, "config", "diff.context", "0")
					gitCmd(t, c.WorkDir, "config", "diff.outputIndicatorNew", "^")
					gitCmd(t, c.WorkDir, "config", "diff.outputIndicatorOld", "v")
					gitCmd(t, c.WorkDir, "config", "diff.outputIndicatorContext", "|")
				}
				if err := os.WriteFile(path, staged, 0600); err != nil {
					t.Fatal(err)
				}
				gitCmd(t, c.WorkDir, "add", "feature.txt")
			} else {
				gitCmd(t, c.WorkDir, "rm", "--cached", "feature.txt")
			}
			if err := os.WriteFile(path, working, 0600); err != nil {
				t.Fatal(err)
			}
			if err := snapshotLeftoverValidationEdits(c, "test", head); err != nil {
				t.Fatal(err)
			}
			files, err := filepath.Glob(filepath.Join(c.RunLogDir, "leftover-validation-*.json"))
			if err != nil || len(files) != 1 {
				t.Fatalf("snapshot: %v %v", files, err)
			}
			data, err := os.ReadFile(files[0])
			if err != nil {
				t.Fatal(err)
			}
			var record struct {
				Paths     [][]byte                 `json:"paths"`
				Status    []byte                   `json:"status"`
				Staged    []byte                   `json:"staged_patch"`
				Unstaged  []byte                   `json:"unstaged_patch"`
				Untracked []leftoverValidationFile `json:"untracked"`
			}
			if err := json.Unmarshal(data, &record); err != nil {
				t.Fatal(err)
			}
			if kind == "deleted index with surviving file" {
				if len(record.Paths) != 1 || string(record.Paths[0]) != "feature.txt" {
					t.Fatalf("paths not deduplicated: %q", record.Paths)
				}
				for _, status := range []string{"D  feature.txt\x00", "?? feature.txt\x00"} {
					if bytes.Count(record.Status, []byte(status)) != 1 {
						t.Fatalf("lost or duplicated status %q: %q", status, record.Status)
					}
				}
				if len(record.Untracked) != 1 || string(record.Untracked[0].Path) != "feature.txt" || !bytes.Equal(record.Untracked[0].Data, working) {
					t.Fatal("lost untracked working copy")
				}
				if bytes.Count(record.Staged, []byte("diff --git a/feature.txt b/feature.txt")) != 1 {
					t.Fatal("duplicated deletion patch")
				}
			}
			if bytes.Contains(record.Staged, []byte{27}) || bytes.Contains(record.Unstaged, []byte{27}) {
				t.Fatal("Git color corrupted recovery patches")
			}
			if err := os.RemoveAll(c.WorkDir); err != nil {
				t.Fatal(err)
			}
			recovery := t.TempDir()
			gitCmd(t, recovery, "clone", c.Repo.UpstreamURL, ".")
			gitCmd(t, recovery, "checkout", "--detach", head)
			for _, patch := range []struct {
				data  []byte
				index bool
			}{{record.Staged, true}, {record.Unstaged, false}} {
				if len(patch.data) == 0 {
					continue
				}
				file := filepath.Join(t.TempDir(), "recover.patch")
				if err := os.WriteFile(file, patch.data, 0600); err != nil {
					t.Fatal(err)
				}
				args := []string{"apply"}
				if patch.index {
					args = append(args, "--index")
				}
				gitCmd(t, recovery, append(args, file)...)
			}
			if kind == "deleted index with surviving file" {
				if err := os.WriteFile(filepath.Join(recovery, "feature.txt"), record.Untracked[0].Data, 0600); err != nil {
					t.Fatal(err)
				}
				if got := gitCmd(t, recovery, "status", "--porcelain"); !strings.Contains(got, "D  feature.txt") || !strings.Contains(got, "?? feature.txt") {
					t.Fatalf("index deletion and survivor not recovered: %q", got)
				}
			} else if got := gitCmd(t, recovery, "show", ":feature.txt"); got != "index version" {
				t.Fatalf("lost index version: %q", got)
			}
			if got, err := os.ReadFile(filepath.Join(recovery, "feature.txt")); err != nil || !bytes.Equal(got, working) {
				t.Fatalf("working copy not recoverable: %q %v", got, err)
			}
		})
	}
}
