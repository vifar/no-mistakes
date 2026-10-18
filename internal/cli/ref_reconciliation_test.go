package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestStatusReportsOrphanWithoutAnyRunOrNotification(t *testing.T) {
	repoDir, p, database, repo := setupAxiQueryRepo(t)
	run(t, repoDir, "git", "clone", "--bare", repoDir, p.RepoDir(repo.ID))
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
