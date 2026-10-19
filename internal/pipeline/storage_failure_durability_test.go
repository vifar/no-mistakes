package pipeline

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
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

func TestStorageFailureFirstUsePublishesDirectoryBeforeMarker(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(fmt.Sprintf("existing=%v", existing), func(t *testing.T) {
			root := t.TempDir()
			directory := filepath.Join(root, "infrastructure-failures")
			path := filepath.Join(directory, "run.json")
			if existing {
				if err := os.Mkdir(directory, 0700); err != nil {
					t.Fatal(err)
				}
			}
			record := storageFailureRecord{"run", "SQLITE_WRITE_CONTENTION: fixture"}
			var synced []string
			err := writeStorageFailureWithSync(path, record, func(dir string) error {
				synced = append(synced, dir)
				switch len(synced) {
				case 1:
					if dir != root {
						t.Fatalf("marker published before its directory entry: first sync=%q want=%q", dir, root)
					}
					if info, err := os.Stat(directory); err != nil || !info.IsDir() {
						t.Fatalf("directory missing before parent sync: %v", err)
					}
					if _, err := os.Stat(path); !os.IsNotExist(err) {
						t.Fatalf("marker created before parent sync: %v", err)
					}
				case 2:
					if dir != directory {
						t.Fatalf("marker rename synced wrong directory: %q", dir)
					}
					data, err := os.ReadFile(path)
					if err != nil {
						t.Fatal(err)
					}
					var got storageFailureRecord
					if err := json.Unmarshal(data, &got); err != nil || got != record {
						t.Fatalf("invalid persisted marker: %+v %v", got, err)
					}
				default:
					t.Fatalf("unexpected sync: %q", dir)
				}
				return syncStorageFailureDirectory(dir)
			})
			if err != nil || len(synced) != 2 {
				t.Fatalf("incomplete durable publication: syncs=%v error=%v", synced, err)
			}
		})
	}
}

func TestStorageFailurePublicationPropagatesDirectorySyncErrors(t *testing.T) {
	for _, failAt := range []int{1, 2} {
		t.Run(fmt.Sprintf("sync=%d", failAt), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "infrastructure-failures", "run.json")
			calls := 0
			var syncFailure error
			err := writeStorageFailureWithSync(path, storageFailureRecord{"run", "SQLITE_WRITE_CONTENTION: fixture"}, func(dir string) error {
				calls++
				if calls != failAt {
					return syncStorageFailureDirectory(dir)
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
			if calls != failAt || syncFailure == nil || !errors.Is(err, syncFailure) {
				t.Fatalf("sync failure hidden: calls=%d sync=%v result=%v", calls, syncFailure, err)
			}
			_, statErr := os.Stat(path)
			if failAt == 1 && !os.IsNotExist(statErr) {
				t.Fatalf("published marker after parent sync failed: %v", statErr)
			}
			if failAt == 2 && statErr != nil {
				t.Fatalf("renamed marker discarded after sync failure: %v", statErr)
			}
		})
	}
}

func TestStorageFailureDirectoryPlatformContract(t *testing.T) {
	for _, platform := range []string{"windows", "darwin", "linux"} {
		t.Run(platform, func(t *testing.T) {
			dir := t.TempDir()
			called := false
			err := syncStorageFailureDirectoryForPlatform(dir, platform, func(f *os.File) error {
				called = true
				if platform == "windows" {
					return os.ErrPermission
				}
				if runtime.GOOS == "windows" {
					_, err := f.Stat()
					return err
				}
				return f.Sync()
			})
			if err != nil || called != (platform != "windows") {
				t.Fatalf("unsupported directory flush: platform=%s called=%v err=%v", platform, called, err)
			}
			missing := filepath.Join(dir, "missing")
			err = syncStorageFailureDirectoryForPlatform(missing, platform, func(*os.File) error { t.Fatal("flushed nonexistent directory"); return nil })
			if !os.IsNotExist(err) {
				t.Fatalf("lost directory open error: %v", err)
			}
			if platform != "windows" {
				failure := errors.New("directory IO failure")
				err := syncStorageFailureDirectoryForPlatform(dir, platform, func(*os.File) error { return failure })
				if !errors.Is(err, failure) {
					t.Fatalf("real sync error swallowed: %v", err)
				}
			}
		})
	}
}

func TestStorageFailureLifecycleOnCurrentPlatform(t *testing.T) {
	path := filepath.Join(t.TempDir(), "infrastructure-failures", "run.json")
	record := storageFailureRecord{"run", "SQLITE_WRITE_CONTENTION: fixture"}
	for i := 0; i < 2; i++ {
		if err := writeStorageFailure(path, record); err != nil {
			t.Fatal(err)
		}
		if err := removeStorageFailure(path, syncStorageFailureDirectory); err != nil {
			t.Fatal(err)
		}
		if err := removeStorageFailure(path, syncStorageFailureDirectory); err != nil {
			t.Fatalf("empty cleanup blocked subsequent execution: %v", err)
		}
	}
}

func TestStorageFailureWindowsLifecycleBoundary(t *testing.T) {
	path := filepath.Join(t.TempDir(), "infrastructure-failures", "run.json")
	record := storageFailureRecord{"run", "SQLITE_WRITE_CONTENTION: fixture"}
	syncDir := func(dir string) error {
		return syncStorageFailureDirectoryForPlatform(dir, "windows", func(*os.File) error {
			t.Fatal("Windows lifecycle attempted unsupported directory flush")
			return os.ErrPermission
		})
	}
	for i := 0; i < 2; i++ {
		if err := writeStorageFailureWithSync(path, record, syncDir); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var got storageFailureRecord
		if err := json.Unmarshal(data, &got); err != nil || got != record {
			t.Fatalf("lost Windows marker: %+v %v", got, err)
		}
		if err := removeStorageFailure(path, syncDir); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("Windows marker not removed: %v", err)
		}
		if err := removeStorageFailure(path, syncDir); err != nil {
			t.Fatalf("Windows cleanup stranded subsequent execution: %v", err)
		}
	}
}
