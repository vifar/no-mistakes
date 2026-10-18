package db

import (
	"database/sql"
	"encoding/json"
	"fmt"
)

// TargetRewindHistory keeps superseded evidence and gate decisions separately
// from executable state. Nothing in these snapshots authorizes publication.
type TargetRewindHistory struct {
	BoundaryStep  *string
	BoundaryJSON  *string
	Comparison    PRContext
	ReviewHeadSHA *string
	Steps         []map[string]any
	Rounds        []map[string]any
}

func snapshotRows(tx *sql.Tx, query, runID string) ([]map[string]any, error) {
	rows, err := tx.Query(query, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	result := []map[string]any{}
	for rows.Next() {
		values := make([]any, len(columns))
		pointers := make([]any, len(columns))
		for i := range values {
			pointers[i] = &values[i]
		}
		if err := rows.Scan(pointers...); err != nil {
			return nil, err
		}
		row := map[string]any{}
		for i, column := range columns {
			row[column] = values[i]
		}
		result = append(result, row)
	}
	return result, rows.Err()
}

func archiveTargetRewind(tx *sql.Tx, id, runID string, comparison *PRContext, reviewHead *string) error {
	steps, err := snapshotRows(tx, `SELECT * FROM step_results WHERE run_id=? ORDER BY step_order,id`, runID)
	if err != nil {
		return err
	}
	rounds, err := snapshotRows(tx, `SELECT r.* FROM step_rounds r JOIN step_results s ON s.id=r.step_result_id WHERE s.run_id=? ORDER BY r.created_at,r.id`, runID)
	if err != nil {
		return err
	}
	c, err := json.Marshal(comparison)
	if err != nil {
		return err
	}
	s, err := json.Marshal(steps)
	if err != nil {
		return err
	}
	r, err := json.Marshal(rounds)
	if err != nil {
		return err
	}
	_, err = tx.Exec(`INSERT INTO target_rewind_history(rewind_id,comparison_json,review_head_sha,steps_json,rounds_json) VALUES(?,?,?,?,?)`, id, string(c), reviewHead, string(s), string(r))
	return err
}

func (d *DB) GetTargetRewindHistory(runID string) ([]TargetRewindHistory, error) {
	rows, err := d.sql.Query(`SELECT h.comparison_json,h.review_head_sha,h.steps_json,h.rounds_json,h.boundary_step,h.boundary_json FROM target_rewind_history h JOIN run_target_rewinds w ON w.id=h.rewind_id WHERE w.run_id=? ORDER BY w.generation`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []TargetRewindHistory
	for rows.Next() {
		var h TargetRewindHistory
		var c, s, r string
		if err := rows.Scan(&c, &h.ReviewHeadSHA, &s, &r, &h.BoundaryStep, &h.BoundaryJSON); err != nil {
			return nil, err
		}
		for _, v := range []struct {
			raw string
			dst any
		}{{c, &h.Comparison}, {s, &h.Steps}, {r, &h.Rounds}} {
			if err := json.Unmarshal([]byte(v.raw), v.dst); err != nil {
				return nil, fmt.Errorf("read target rewind history: %w", err)
			}
		}
		result = append(result, h)
	}
	return result, rows.Err()
}

// RecordTargetRewindBoundary preserves a result produced just before the
// post-step guard rewound it. It is diagnostic until Git proves reuse safe.
func (d *DB) RecordTargetRewindBoundary(runID string, generation int64, step string, outcome string) error {
	result, err := d.sql.Exec(`UPDATE target_rewind_history SET boundary_step=?,boundary_json=? WHERE rewind_id IN (SELECT id FROM run_target_rewinds WHERE run_id=? AND generation=(SELECT MAX(generation) FROM run_target_rewinds WHERE run_id=?)) AND (SELECT target_advance_rewinds FROM runs WHERE id=?)=?`, step, outcome, runID, runID, runID, generation)
	if err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		return fmt.Errorf("target rewind boundary has no matching generation")
	}
	return nil
}
