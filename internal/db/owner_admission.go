package db

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const ownerAdmissionSchema = `
CREATE TABLE IF NOT EXISTS owner_admission_events (
 run_id TEXT PRIMARY KEY REFERENCES runs(id) ON DELETE CASCADE,
 payload TEXT NOT NULL
);
`

// GetOwnerAdmission returns the recorded admission, never an inference from
// current config or log files. The event and run are inserted atomically.
func (d *DB) GetOwnerAdmission(runID string) (*types.OwnerAdmissionEvent, error) {
	var payload string
	err := d.sql.QueryRow("SELECT payload FROM owner_admission_events WHERE run_id=?", runID).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read owner admission: %w", err)
	}
	var event types.OwnerAdmissionEvent
	if err := json.Unmarshal([]byte(payload), &event); err != nil {
		return nil, fmt.Errorf("decode owner admission: %w", err)
	}
	return &event, nil
}
