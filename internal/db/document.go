package db

import (
	"context"
	"fmt"

	"github.com/kunchenguid/no-mistakes/internal/sqlitewrite"
)

// DocumentPass survives step resets. A missing ToHeadSHA means an invocation
// was interrupted; recovery must not silently give it another commit budget.
type DocumentPass struct {
	ID                string
	FromHeadSHA       string
	ToHeadSHA         *string
	CommitCount       int
	FindingsJSON      *string
	CarriedReviewFrom *string
}

func (d *DB) GetDocumentPasses(runID string) ([]DocumentPass, error) {
	rows, err := d.sql.Query(`SELECT id, from_head_sha, to_head_sha, commit_count, findings_json, carried_review_from FROM document_passes WHERE run_id=? ORDER BY id`, runID)
	if err != nil {
		return nil, fmt.Errorf("read document passes: %w", err)
	}
	defer rows.Close()
	var passes []DocumentPass
	for rows.Next() {
		var p DocumentPass
		if err := rows.Scan(&p.ID, &p.FromHeadSHA, &p.ToHeadSHA, &p.CommitCount, &p.FindingsJSON, &p.CarriedReviewFrom); err != nil {
			return nil, err
		}
		passes = append(passes, p)
	}
	return passes, rows.Err()
}

func (d *DB) BeginDocumentPass(runID, from string) (string, error) {
	if !validGitSHA(from) {
		return "", fmt.Errorf("invalid Document starting head")
	}
	id := newID()
	_, err := sqlitewrite.Exec(context.Background(), d.sql, "BeginDocumentPass", `INSERT INTO document_passes(id,run_id,from_head_sha) VALUES(?,?,?)`, id, runID, from)
	if err != nil {
		return "", fmt.Errorf("begin document pass: %w", err)
	}
	return id, nil
}

func (d *DB) CompleteDocumentPass(id, to string, commits int, findings string) error {
	if !validGitSHA(to) || commits < 0 {
		return fmt.Errorf("invalid document pass result")
	}
	result, err := sqlitewrite.Exec(context.Background(), d.sql, "CompleteDocumentPass", `UPDATE document_passes SET to_head_sha=?,commit_count=?,findings_json=? WHERE id=? AND to_head_sha IS NULL`, to, commits, findings, id)
	if err != nil {
		return fmt.Errorf("complete document pass: %w", err)
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		return fmt.Errorf("complete document pass: result no longer pending (%d rows): %v", n, err)
	}
	return nil
}
