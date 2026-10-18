package db

import (
	"database/sql"
	"errors"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestTargetRewindRetargetProvenanceOnUpgrade(t *testing.T) {
	for _, kind := range []string{"legacy unknown", "legacy then attempted", "new untouched", "new attempted"} {
		t.Run(kind, func(t *testing.T) {
			d, id, candidate, request := targetRewindDBFixture(t)
			run, err := d.GetRun(id)
			if err != nil {
				t.Fatal(err)
			}
			previous, err := d.GetRunPRContext(id)
			if err != nil {
				t.Fatal(err)
			}
			attached := previous.PRContextCandidate
			attached.PRURL = "https://gitlab.example/acme/repo/-/merge_requests/42"
			attached.ForgeHeadSHA = attached.LocalHeadSHA
			bound, err := d.BindRunPRContext(id, attached, types.StepRebase)
			if err != nil {
				t.Fatal(err)
			}
			candidate.PRURL, candidate.ForgeHeadSHA = attached.PRURL, attached.ForgeHeadSHA
			request.ExpectedGeneration, request.HasPR = bound.Generation, true
			if allowed, err := d.CanRewindTarget(id, request.Step); err != nil || !allowed {
				t.Fatalf("new untouched run must be eligible: %t %v", allowed, err)
			}
			if kind == "legacy unknown" || kind == "legacy then attempted" {
				if _, err := d.sql.Exec(`ALTER TABLE runs DROP COLUMN retarget_attempted; PRAGMA user_version=0`); err != nil {
					t.Fatal(err)
				}
			}
			var sequence int
			var name, path string
			if err := d.sql.QueryRow(`PRAGMA database_list`).Scan(&sequence, &name, &path); err != nil {
				t.Fatal(err)
			}
			if err := d.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			if kind == "legacy then attempted" || kind == "new attempted" {
				if err := reopened.MarkRunRetargetAttempted(id); err != nil {
					t.Fatal(err)
				}
			}
			var stored sql.NullBool
			if err := reopened.sql.QueryRow(`SELECT retarget_attempted FROM runs WHERE id=?`, id).Scan(&stored); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "legacy unknown":
				if stored.Valid {
					t.Errorf("legacy migration fabricated retarget provenance: %+v", stored)
				}
			case "new untouched":
				if !stored.Valid || stored.Bool {
					t.Errorf("new run lacks confirmed absence: %+v", stored)
				}
			default:
				if !stored.Valid || !stored.Bool {
					t.Errorf("forward-recorded attempt was lost: %+v", stored)
				}
			}
			wantAllowed := kind == "new untouched"
			if allowed, err := reopened.CanRewindTarget(id, request.Step); err != nil || allowed != wantAllowed {
				t.Errorf("approved eligibility contract requires confirmed absence: allowed=%t want=%t error=%v", allowed, wantAllowed, err)
			}
			_, err = reopened.RewindRunPRContext(id, candidate, request)
			if wantAllowed {
				if err != nil {
					t.Fatal(err)
				}
				assertRewindCharge(t, reopened, id, 1, true)
			} else {
				if !errors.Is(err, ErrTargetAdvanceIneligible) {
					t.Errorf("transaction accepted unproven absence: %v", err)
				}
				assertRewindCharge(t, reopened, id, 0, false)
				receipt, err := reopened.GetRunPRContext(id)
				if err != nil || receipt.Generation != bound.Generation || receipt.PRContextCandidate != attached {
					t.Fatalf("refusal changed the bound comparison: %+v %v", receipt, err)
				}
			}
			fresh, err := reopened.InsertRun(run.RepoID, "fresh", candidate.LocalHeadSHA, candidate.TargetSHA)
			if err != nil {
				t.Fatal(err)
			}
			if allowed, err := reopened.CanRewindTarget(fresh.ID, types.StepTest); err != nil || !allowed {
				t.Fatalf("new run after upgrade inherited legacy refusal: %t %v", allowed, err)
			}
		})
	}
}
