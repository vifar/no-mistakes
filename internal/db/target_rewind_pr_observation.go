package db

import (
	"database/sql"
	"errors"
	"fmt"
	"net/url"
)

// TargetRewindPRObservation pins discovery only; it never attaches the PR to
// the run and cannot authorize a PR mutation before the local head is published.
type TargetRewindPRObservation struct{ PRURL, HeadSHA string }

func targetRewindPRObservation(q rewindQuery, runID string) (*TargetRewindPRObservation, error) {
	var observation TargetRewindPRObservation
	err := q.QueryRow(`SELECT pr_url,head_sha FROM run_rewind_pr_observations WHERE run_id=?`, runID).Scan(&observation.PRURL, &observation.HeadSHA)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &observation, nil
}

func (d *DB) GetTargetRewindPRObservation(runID string) (*TargetRewindPRObservation, error) {
	return targetRewindPRObservation(d.sql, runID)
}

// PinTargetRewindPRObservation is called only for the first comparison. A
// later discovery must never be turned into the initial observation.
func (d *DB) PinTargetRewindPRObservation(runID, prURL, headSHA string) error {
	parsed, err := url.Parse(prURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil || !validGitSHA(headSHA) {
		return fmt.Errorf("invalid initial PR observation")
	}
	result, err := d.sql.Exec(`INSERT INTO run_rewind_pr_observations(run_id,pr_url,head_sha)
 SELECT run_id,?,? FROM run_pr_contexts WHERE run_id=? AND generation=1 AND pr_url IS NULL
 ON CONFLICT(run_id) DO NOTHING`, prURL, headSHA, runID)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	observation, err := d.GetTargetRewindPRObservation(runID)
	if err != nil {
		return err
	}
	if observation == nil || observation.PRURL != prURL || observation.HeadSHA != headSHA {
		return fmt.Errorf("initial PR observation cannot be replaced (inserted=%d)", n)
	}
	return nil
}
