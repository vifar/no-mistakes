package db

import (
	"database/sql"
	"errors"
	"fmt"
)

const pushNotificationSchema = `CREATE TABLE IF NOT EXISTS push_notifications (
 notification_id TEXT PRIMARY KEY,
 run_id TEXT REFERENCES runs(id),
 diagnostic_run_id TEXT REFERENCES runs(id)
);`

type RunCreation struct {
	NotificationID   string
	PRURL            string
	ClosingIssueRefs []string
	DiagnosticError  string
}

func (d *DB) PushNotificationRuns(id string) (admitted, diagnostic *Run, err error) {
	var runID, diagnosticID sql.NullString
	err = d.sql.QueryRow("SELECT run_id,diagnostic_run_id FROM push_notifications WHERE notification_id=?", id).Scan(&runID, &diagnosticID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	if runID.Valid {
		admitted, err = d.GetRun(runID.String)
		if err != nil {
			return nil, nil, err
		}
		if admitted == nil {
			return nil, nil, fmt.Errorf("notification admission run missing")
		}
	}
	if diagnosticID.Valid {
		diagnostic, err = d.GetRun(diagnosticID.String)
	}
	return
}

func (d *DB) BindPushNotification(id, runID string) error {
	_, err := d.sql.Exec(`INSERT INTO push_notifications(notification_id,run_id) VALUES (?,?)
 ON CONFLICT(notification_id) DO UPDATE SET run_id=excluded.run_id WHERE push_notifications.run_id IS NULL`, id, runID)
	if err != nil {
		return err
	}
	run, _, err := d.PushNotificationRuns(id)
	if err != nil {
		return err
	}
	if run == nil || run.ID != runID {
		return fmt.Errorf("conflicting notification admission")
	}
	return nil
}
