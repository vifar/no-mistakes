package db

import (
	"context"
	"fmt"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestSuccessfulTerminalCommitStatusRequiresValidation(t *testing.T) {
	for _, status := range []types.RunStatus{types.RunCompleted, types.RunChecksPassed} {
		for _, validated := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/validated=%v", status, validated), func(t *testing.T) {
				d := openTestDB(t)
				repo, err := d.InsertRepo("fixture", "https://github.com/test/repo", "main")
				if err != nil {
					t.Fatal(err)
				}
				run, err := d.InsertRun(repo.ID, "feature", "head", "base")
				if err != nil {
					t.Fatal(err)
				}
				publication := CommitStatusPublication{SHA: "head", Host: "github.com", Repo: "test/repo", LastState: "pending", Validated: validated}
				if err := d.PutCommitStatusPublication(run.ID, publication); err != nil {
					t.Fatal(err)
				}
				if err := d.UpdateRunStatus(run.ID, status); err != nil {
					t.Fatal(err)
				}
				ids, err := d.CommitStatusRecoveryRunIDs(context.Background())
				if err != nil || len(ids) != 1 || ids[0] != run.ID {
					t.Fatalf("pending terminal publication not selected: %v, %v", ids, err)
				}
				for _, state := range []string{"pending", "success", "failure"} {
					publication.LastState = state
					err := d.PutCommitStatusPublication(run.ID, publication)
					allowed := state == "failure" && !validated || state == "success" && validated
					if (err == nil) != allowed {
						t.Fatalf("state=%s validated=%v: %v", state, validated, err)
					}
				}
				ids, err = d.CommitStatusRecoveryRunIDs(context.Background())
				if err != nil || len(ids) != 0 {
					t.Fatalf("settled publication still needs recovery: %v, %v", ids, err)
				}
			})
		}
	}
}
