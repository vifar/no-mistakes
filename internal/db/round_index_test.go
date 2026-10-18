package db

import (
	"path/filepath"
	"strings"
	"testing"
)

func roundsByStepQueryPlan(t *testing.T, d *DB) string {
	t.Helper()
	return roundsQueryPlan(t, d, "EXPLAIN QUERY PLAN "+roundsByStepQuery)
}

func roundsQueryPlan(t *testing.T, d *DB, query string) string {
	t.Helper()
	rows, err := d.sql.Query(query, "step-1")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var details []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		details = append(details, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return strings.Join(details, "; ")
}

func assertRoundsByStepIndex(t *testing.T, d *DB) {
	t.Helper()
	plan := roundsByStepQueryPlan(t, d)
	t.Logf("GetRoundsByStep query plan: %s", plan)
	if !strings.Contains(plan, "USING INDEX idx_step_rounds_step_result_round") {
		t.Errorf("query plan does not use the step rounds index: %s", plan)
	}
	if strings.Contains(strings.ToUpper(plan), "SCAN STEP_ROUNDS") {
		t.Errorf("query plan scans all step rounds: %s", plan)
	}
	if strings.Contains(strings.ToUpper(plan), "TEMP B-TREE") {
		t.Errorf("query plan sorts step rounds in a temp b-tree: %s", plan)
	}
}

func TestStepRoundsQueryPlan(t *testing.T) {
	assertRoundsByStepIndex(t, openTestDB(t))
}

func TestStepRoundsIndexExistingDatabase(t *testing.T) {
	for _, hotfixed := range []bool{false, true} {
		name := "upgrade"
		if hotfixed {
			name = "hotfixed"
		}
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "existing.sqlite")
			d, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if d != nil {
					d.Close()
				}
			}()
			if _, err := d.sql.Exec(`DROP INDEX IF EXISTS idx_step_rounds_step_result_round; PRAGMA user_version=0`); err != nil {
				t.Fatal(err)
			}
			t.Logf("old schema query plan: %s", roundsByStepQueryPlan(t, d))
			if hotfixed {
				if _, err := d.sql.Exec(`CREATE INDEX idx_step_rounds_step_result_round ON step_rounds(step_result_id, round)`); err != nil {
					t.Fatal(err)
				}
			}
			for range 2 {
				if err := d.Close(); err != nil {
					t.Fatal(err)
				}
				d, err = Open(path)
				if err != nil {
					t.Fatal(err)
				}
				assertRoundsByStepIndex(t, d)
			}
		})
	}
}

// GetStepsByRun runs once per run on every status and run-list request, so it
// must seek a run's steps instead of scanning every step result.
func TestStepResultsByRunQueryPlan(t *testing.T) {
	d := openTestDB(t)
	plan := roundsQueryPlan(t, d, "EXPLAIN QUERY PLAN "+d.stepsByRunQuery())
	t.Logf("GetStepsByRun query plan: %s", plan)
	if !strings.Contains(plan, "USING INDEX idx_step_results_run_order") {
		t.Errorf("query plan does not use the step results index: %s", plan)
	}
	if strings.Contains(strings.ToUpper(plan), "SCAN STEP_RESULTS") {
		t.Errorf("query plan scans all step results: %s", plan)
	}
	if strings.Contains(strings.ToUpper(plan), "TEMP B-TREE") {
		t.Errorf("query plan sorts step results in a temp b-tree: %s", plan)
	}
}
