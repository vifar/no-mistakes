package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestStatusReportsOrphanWithoutAnyRunOrNotification(t *testing.T) {
	// The shared fixture now enrolls the gate itself: it clones the working
	// repository into the gate path, isolates the gate's hooks and records the
	// shared-config baseline. This test must not clone over that gate again.
	// The enrolled gate already carries refs/heads/main at the working
	// repository's HEAD, which is the orphaned ref this test reports.
	repoDir, _, database, repo := setupAxiQueryRepo(t)
	head, err := git.Run(t.Context(), repoDir, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	chdir(t, repoDir)
	for _, status := range []string{"axi", "human"} {
		t.Run(status, func(t *testing.T) {
			var out string
			if status == "axi" {
				out = axiStatusOutput(t, "")
			} else {
				var buf bytes.Buffer
				cmd := newStatusCmd()
				cmd.SetOut(&buf)
				if err := cmd.Execute(); err != nil {
					t.Fatal(err)
				}
				out = buf.String()
			}
			for _, want := range []string{"STARTUP_ORPHANED_REF", "refs/heads/main", head} {
				if !strings.Contains(out, want) {
					t.Fatalf("%s status missing %q: %s", status, want, out)
				}
			}
		})
	}
	recorded, err := database.InsertRun(repo.ID, "main", head, head)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunStatus(recorded.ID, types.RunCompleted); err != nil {
		t.Fatal(err)
	}
	if out := axiStatusOutput(t, ""); strings.Contains(out, "STARTUP_ORPHANED_REF") {
		t.Fatalf("status did not clear after run admission: %s", out)
	}
}
