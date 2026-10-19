package pipeline

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestStorageFailureRemovalSyncsDirectory(t *testing.T) {
	for _, state := range []string{"present", "missing_marker", "missing_parent"} {
		t.Run(state, func(t *testing.T) {
			parent := filepath.Join(t.TempDir(), "infrastructure-failures")
			path := filepath.Join(parent, "run.json")
			if state != "missing_parent" {
				if err := os.Mkdir(parent, 0700); err != nil {
					t.Fatal(err)
				}
			}
			if state == "present" {
				if err := writeStorageFailure(path, storageFailureRecord{"run", "SQLITE_WRITE_CONTENTION: old failure"}); err != nil {
					t.Fatal(err)
				}
			}
			synced := false
			err := removeStorageFailure(path, func(dir string) error {
				synced = true
				if dir != parent {
					t.Fatalf("synced wrong directory: %q", dir)
				}
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatalf("sync preceded deletion: %v", err)
				}
				return syncStorageFailureDirectory(dir)
			})
			if err != nil || !synced {
				t.Fatalf("removal did not synchronize parent: synced=%v err=%v", synced, err)
			}
		})
	}
}

func TestStorageFailureRemovalReportsSyncFailureAndRetriesMissingMarker(t *testing.T) {
	parent := t.TempDir()
	path := filepath.Join(parent, "run.json")
	if err := writeStorageFailure(path, storageFailureRecord{"run", "SQLITE_WRITE_CONTENTION: old failure"}); err != nil {
		t.Fatal(err)
	}
	var syncFailure error
	err := removeStorageFailure(path, func(dir string) error {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("marker still exists at sync boundary: %v", err)
		}
		f, err := os.Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
		syncFailure = f.Sync()
		return syncFailure
	})
	if syncFailure == nil || !errors.Is(err, syncFailure) {
		t.Fatalf("directory sync failure was hidden: sync=%v result=%v", syncFailure, err)
	}
	synced := false
	if err := removeStorageFailure(path, func(dir string) error {
		synced = true
		return syncStorageFailureDirectory(dir)
	}); err != nil || !synced {
		t.Fatalf("retry failed to sync prior deletion: synced=%v err=%v", synced, err)
	}
}

func TestStorageFailureRemovalReportsUnlinkFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "run.json")
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "unexpected"), []byte("data"), 0600); err != nil {
		t.Fatal(err)
	}
	err := removeStorageFailure(path, func(string) error { t.Fatal("synced after failed unlink"); return nil })
	if err == nil {
		t.Fatal("failed marker removal reported success")
	}
}
