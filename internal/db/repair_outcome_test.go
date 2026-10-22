package db

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestRepairOutcomeRoundPersistenceAndUpgrade(t *testing.T) {
	for _, version := range []int{0, schemaVersion - 1} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.sqlite")
			d, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			repo, _ := d.InsertRepo("/fixture", "https://example.com/repo", "main")
			run, _ := d.InsertRun(repo.ID, "feature", "head", "base")
			step, _ := d.InsertStepResult(run.ID, types.StepReview)
			description := "no changes applied"
			if _, err := d.InsertReviewStepRound(step.ID, 1, "auto_fix", nil, &description, "head", 1); err != nil {
				t.Fatal(err)
			}
			if _, err := d.sql.Exec(fmt.Sprintf("ALTER TABLE step_rounds DROP COLUMN fix_applied; PRAGMA user_version=%d", version)); err != nil {
				t.Fatal(err)
			}
			d.Close()
			d, err = Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			for i, applied := range []bool{true, false} {
				if _, err := d.InsertReviewStepRound(step.ID, i+2, "auto_fix", nil, &description, "head", 1, &applied); err != nil {
					t.Fatal(err)
				}
			}
			byStep, err := d.GetRoundsByStep(step.ID)
			if err != nil {
				t.Fatal(err)
			}
			byRun, err := d.GetRoundsByRun(run.ID)
			if err != nil {
				t.Fatal(err)
			}
			for _, rounds := range [][]*StepRound{byStep, byRun} {
				if len(rounds) != 3 || rounds[0].FixApplied != nil || rounds[1].FixApplied == nil || !*rounds[1].FixApplied || rounds[2].FixApplied == nil || *rounds[2].FixApplied {
					t.Fatalf("repair evidence not preserved: %+v", rounds)
				}
				for _, r := range rounds {
					if r.FixSummary == nil || *r.FixSummary != description {
						t.Fatal("description changed")
					}
				}
			}
		})
	}
}
