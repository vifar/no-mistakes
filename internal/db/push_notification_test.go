package db

import (
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestPushNotificationVersionTwoUpgradeAndAtomicBinding(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.sqlite")
	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	repo, err := d.InsertRepo("fixture", "https://example.com/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.sql.Exec("DROP TABLE push_notifications; PRAGMA user_version=2"); err != nil {
		t.Fatal(err)
	}
	if err := migrate(d.sql); err != nil {
		t.Fatal(err)
	}
	event := &types.OwnerAdmissionEvent{Event: "launch_owner_policy_bypassed", OwnerlessAdmitted: true}
	creation := RunCreation{NotificationID: "retained", ClosingIssueRefs: []string{"67"}, PRURL: "https://example.com/pr/66"}
	run, err := d.InsertRunWithAllOptions(repo.ID, "fresh", "head", "base", &RunIntent{OwnerAdmission: event}, "", "", "", "", false, false, "", nil, nil, nil, creation)
	if err != nil {
		t.Fatal(err)
	}
	bound, _, err := d.PushNotificationRuns("retained")
	if err != nil || bound == nil || bound.ID != run.ID || len(bound.ClosingIssueRefs) != 1 || bound.PRURL == nil {
		t.Fatalf("missing creation metadata: %+v %v", bound, err)
	}
	if _, err := d.sql.Exec("CREATE TRIGGER reject_binding BEFORE INSERT ON push_notifications BEGIN SELECT RAISE(ABORT,'binding failure'); END"); err != nil {
		t.Fatal(err)
	}
	creation.NotificationID = "second"
	if _, err := d.InsertRunWithAllOptions(repo.ID, "fresh", "head2", "base", &RunIntent{OwnerAdmission: event}, "", "", "", "", false, false, "", nil, nil, nil, creation); err == nil {
		t.Fatal("binding failure silently admitted run")
	}
	rows, err := d.GetRunsByRepo(repo.ID)
	if err != nil || len(rows) != 1 {
		t.Fatalf("binding failure escaped transaction: %+v %v", rows, err)
	}
	var n int
	if err := d.sql.QueryRow("SELECT count(*) FROM owner_admission_events").Scan(&n); err != nil || n != 1 {
		t.Fatalf("event escaped transaction: %d %v", n, err)
	}
}

func TestDeleteRepoPushNotificationsAtomic(t *testing.T) {
	for _, failDelete := range []bool{false, true} {
		name := "success"
		if failDelete {
			name = "rollback"
		}
		t.Run(name, func(t *testing.T) {
			d := openTestDB(t)
			repo, err := d.InsertRepo("target", "origin", "main")
			if err != nil {
				t.Fatal(err)
			}
			other, err := d.InsertRepo("unrelated", "origin", "main")
			if err != nil {
				t.Fatal(err)
			}
			for _, r := range []*Repo{repo, other} {
				event := &types.OwnerAdmissionEvent{Event: "launch_owner_policy_bypassed", OwnerlessAdmitted: true}
				admitted, err := d.InsertRunWithAllOptions(r.ID, "feature", "head", "base", &RunIntent{OwnerAdmission: event}, "", "", "", "", false, false, "", nil, nil, nil, RunCreation{NotificationID: r.ID + "-admitted"})
				if err != nil {
					t.Fatal(err)
				}
				diagnostic, err := d.InsertRunWithAllOptions(r.ID, "feature", "head", "base", nil, "", "", "", "", false, false, "", nil, nil, nil, RunCreation{NotificationID: r.ID + "-diagnostic", DiagnosticError: "setup failed"})
				if err != nil {
					t.Fatal(err)
				}
				if err := d.BindPushDiagnostic(r.ID+"-both", diagnostic.ID); err != nil {
					t.Fatal(err)
				}
				if err := d.BindPushNotification(r.ID+"-both", admitted.ID); err != nil {
					t.Fatal(err)
				}
				step, err := d.InsertStepResult(admitted.ID, types.StepReview)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := d.InsertStepRound(step.ID, 1, "initial", nil, nil, 0); err != nil {
					t.Fatal(err)
				}
			}
			if failDelete {
				if _, err := d.sql.Exec("CREATE TRIGGER reject_repo_delete BEFORE DELETE ON repos BEGIN SELECT RAISE(ABORT,'repo deletion blocked'); END"); err != nil {
					t.Fatal(err)
				}
			}
			err = d.DeleteRepo(repo.ID)
			if failDelete {
				if err == nil {
					t.Fatal("expected repository deletion failure")
				}
			} else if err != nil {
				t.Fatalf("delete repository with admitted and diagnostic notifications: %v", err)
			}
			for _, r := range []*Repo{repo, other} {
				retained := r.ID == other.ID || failDelete
				wantRepos, wantRuns, wantNotifications, wantEvents, wantSteps := 0, 0, 0, 0, 0
				if retained {
					wantRepos, wantRuns, wantNotifications, wantEvents, wantSteps = 1, 2, 3, 1, 1
				}
				checks := []struct {
					query string
					want  int
				}{
					{"SELECT count(*) FROM repos WHERE id=?", wantRepos},
					{"SELECT count(*) FROM runs WHERE repo_id=?", wantRuns},
					{"SELECT count(*) FROM push_notifications WHERE notification_id IN (? || '-admitted', ? || '-diagnostic', ? || '-both')", wantNotifications},
					{"SELECT count(*) FROM owner_admission_events WHERE run_id IN (SELECT id FROM runs WHERE repo_id=?)", wantEvents},
					{"SELECT count(*) FROM step_results WHERE run_id IN (SELECT id FROM runs WHERE repo_id=?)", wantSteps},
					{"SELECT count(*) FROM step_rounds WHERE step_result_id IN (SELECT id FROM step_results WHERE run_id IN (SELECT id FROM runs WHERE repo_id=?))", wantSteps},
				}
				for i, check := range checks {
					args := []any{r.ID}
					if i == 2 {
						args = []any{r.ID, r.ID, r.ID}
					}
					var got int
					if err := d.sql.QueryRow(check.query, args...).Scan(&got); err != nil {
						t.Fatal(err)
					}
					if got != check.want {
						t.Errorf("repo %s query %s: got %d, want %d", r.WorkingPath, check.query, got, check.want)
					}
				}
			}
			var violations int
			rows, err := d.sql.Query("PRAGMA foreign_key_check")
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			for rows.Next() {
				violations++
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			if violations != 0 {
				t.Fatalf("foreign key violations: %d", violations)
			}
		})
	}
}
