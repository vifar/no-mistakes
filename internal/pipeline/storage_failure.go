package pipeline

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/sqlitewrite"
)

// The independent marker covers the case where SQLite is still unavailable
// when failRun tries to save its infrastructure outcome. Without it, stale-run
// recovery could replace that outcome with a generic crash and POST failure.
// It contains no SQL, arguments or credentials. It is outside log/evidence
// retention so a reaper cannot erase the recovery discriminator.
type storageFailureRecord struct {
	RunID string `json:"run_id"`
	Error string `json:"error"`
}

func (e *Executor) storageFailurePath(runID string) string {
	if e.paths == nil {
		return ""
	}
	return filepath.Join(e.paths.Root(), "infrastructure-failures", runID+".json")
}

func (e *Executor) recordStorageFailure(run *db.Run, failure error) {
	var contention *sqlitewrite.ContentionError
	if !errors.As(failure, &contention) {
		return
	}
	// Persist only the typed storage error at a canonical prefix. Arbitrary
	// command output mentioning its name must never suppress a code failure.
	message := contention.Error()
	run.Error = &message
	path := e.storageFailurePath(run.ID)
	if path == "" {
		return
	}
	if err := writeStorageFailure(path, storageFailureRecord{run.ID, message}); err != nil {
		slog.Error("SQLITE_INFRASTRUCTURE_RECORD_FAILED: could not retain infrastructure outcome outside SQLite", "run", run.ID, "error", err)
	}
}

func writeStorageFailure(path string, record storageFailureRecord) error {
	return writeStorageFailureWithSync(path, record, syncStorageFailureDirectory)
}

func writeStorageFailureWithSync(path string, record storageFailureRecord, syncDir func(string) error) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	if err := syncDir(filepath.Dir(filepath.Dir(path))); err != nil {
		return err
	}
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".sqlite-write-contention-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return err
	}
	return syncDir(filepath.Dir(path))
}

func (e *Executor) storageFailure(run *db.Run) bool {
	if run.Error != nil && sqlitewrite.RecordedContention(*run.Error) {
		return true
	}
	path := e.storageFailurePath(run.ID)
	if path == "" {
		return false
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return false
	}
	if err != nil {
		slog.Error("SQLITE_INFRASTRUCTURE_RECORD_UNREADABLE: suppressing failure status", "run", run.ID, "error", err)
		return true
	}
	var record storageFailureRecord
	if err := json.Unmarshal(data, &record); err != nil || record.RunID != run.ID || !sqlitewrite.RecordedContention(record.Error) {
		slog.Error("SQLITE_INFRASTRUCTURE_RECORD_INVALID: suppressing failure status", "run", run.ID)
		return true
	}
	return true
}

func (e *Executor) clearStorageFailure(runID string) error {
	path := e.storageFailurePath(runID)
	if path == "" {
		return nil
	}
	return removeStorageFailure(path, syncStorageFailureDirectory)
}

func syncStorageFailureDirectory(path string) error {
	return syncStorageFailureDirectoryForPlatform(path, runtime.GOOS, (*os.File).Sync)
}

func syncStorageFailureDirectoryForPlatform(path, platform string, syncDir func(*os.File) error) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	if platform == "windows" {
		return dir.Close()
	}
	return errors.Join(syncDir(dir), dir.Close())
}

func removeStorageFailure(path string, syncDir func(string) error) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("clear SQLite infrastructure outcome: %w", err)
	}
	if err := syncDir(filepath.Dir(path)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("sync cleared SQLite infrastructure outcome: %w", err)
	}
	return nil
}
