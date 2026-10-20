package gatecontext

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A metadata read that the kernel holds must not hold the caller.
//
// On macOS a process that a service manager started has no responsible
// application, so an `open()` of a file under a privacy-protected location is
// answered only when the sandbox approval watchdog expires. The non-blocking
// open flag does not help: the kernel holds the approval, not the file.
// Measured on the Studio, an `O_NONBLOCK` open of a file under ~/Documents was
// still unanswered after two and a half minutes.
//
// The wait is therefore the only defence, and it must abandon the read rather
// than wait for it. The hold itself is a kernel hold, which no local file can
// reproduce, so the reader is stood in for.
//
// Regressions: issue #120 review finding on the shared discovery boundary.
func TestReadMetadataWithinAbandonsAHeldRead(t *testing.T) {
	release := make(chan struct{})
	original := metadataReadHook
	metadataReadHook = func(string) ([]byte, error) {
		<-release
		return nil, os.ErrDeadlineExceeded
	}
	t.Cleanup(func() {
		metadataReadHook = original
		close(release)
	})

	started := time.Now()
	if _, err := readMetadataWithin(context.Background(), "/any/metadata", 200*time.Millisecond); err == nil {
		t.Fatal("a read that never completed was reported as available")
	}
	elapsed := time.Since(started)
	if elapsed > 2*time.Second {
		t.Fatalf("the wait did not bound the read: %v", elapsed)
	}
}

// A metadata read that completes returns its content.
func TestReadMetadataWithinReturnsContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "commondir")
	if err := os.WriteFile(path, []byte("../..\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	data, err := readMetadataWithin(context.Background(), path, time.Second)
	if err != nil {
		t.Fatal("a readable metadata file was reported as unavailable")
	}
	if string(data) != "../..\n" {
		t.Fatalf("metadata content = %q", data)
	}
}

// A metadata file that is not a regular file is refused rather than read, and
// it is refused without being opened: opening a FIFO waits for a writer.
func TestReadMetadataWithinRefusesANonRegularFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "device")
	if err := os.Symlink(filepath.Join(dir, "nowhere"), path); err != nil {
		t.Fatal(err)
	}
	if _, err := readMetadataWithin(context.Background(), path, time.Second); err == nil {
		t.Fatal("a non-regular file was read as a metadata file")
	}
}

// A caller that has already cancelled must not start or accept a metadata read.
func TestMetadataDiscoveryHonorsCallerCancellation(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "HEAD"), []byte("ref: refs/heads/main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := gitHeadPresent(ctx, dir); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled HEAD read: %v", err)
	}
}

// Cancellation must release a discovery caller while the underlying read is held.
func TestMetadataDiscoveryDeadlineInterruptsHeldHead(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "HEAD"), []byte("ref: refs/heads/main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	finished := make(chan struct{})
	original := metadataReadHook
	metadataReadHook = func(string) ([]byte, error) { defer close(finished); <-release; return nil, os.ErrDeadlineExceeded }
	t.Cleanup(func() { close(release); <-finished; metadataReadHook = original })
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := gitHeadPresent(ctx, dir)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("held HEAD lost caller deadline: %v", err)
	}
}

// An unreadable linked-worktree pointer cannot be replaced with a guess based
// on the name of its parent directory, even when that parent looks like Git.
func TestCommonDirectoryNeverInfersAnUnreadablePointer(t *testing.T) {
	for _, content := range []string{"", "\n", "../missing\n", string(make([]byte, metadataFileLimit+1))} {
		t.Run(fmt.Sprintf("length-%d", len(content)), func(t *testing.T) {
			root := t.TempDir()
			actual := filepath.Join(root, "worktrees", "linked")
			if err := os.MkdirAll(actual, 0o700); err != nil {
				t.Fatal(err)
			}
			for _, entry := range []string{"objects", "refs"} {
				if err := os.Mkdir(filepath.Join(root, entry), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(filepath.Join(actual, "commondir"), []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			common, err := commonDirectoryFor(context.Background(), actual, false)
			if err == nil {
				t.Fatalf("invalid pointer selected %q", common)
			}
		})
	}
}

func TestCommonDirectoryHeldPointerHonorsContext(t *testing.T) {
	root := t.TempDir()
	actual := filepath.Join(root, "worktrees", "linked")
	if err := os.MkdirAll(actual, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(actual, "commondir"), []byte("../..\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	release, finished := make(chan struct{}), make(chan struct{})
	original := metadataReadHook
	metadataReadHook = func(string) ([]byte, error) { defer close(finished); <-release; return []byte("../..\n"), nil }
	t.Cleanup(func() { close(release); <-finished; metadataReadHook = original })
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	common, err := commonDirectoryFor(ctx, actual, false)
	if common != "" || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("held pointer returned common=%q err=%v", common, err)
	}
}

// An unreadable nearer HEAD must not let an enclosing repository replace the
// unresolved boundary. Otherwise a successful Git absence could admit it.
func TestNearestMetadataDoesNotSkipAnUnreadableHead(t *testing.T) {
	root := t.TempDir()
	inner := filepath.Join(root, "inner")
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(inner, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(inner, "HEAD"), []byte("ref: refs/heads/main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	original := metadataReadHook
	metadataReadHook = func(string) ([]byte, error) { return nil, os.ErrPermission }
	t.Cleanup(func() { metadataReadHook = original })
	actual, _, err := nearestGitMetadata(context.Background(), inner)
	if actual != "" || !errors.Is(err, os.ErrPermission) {
		t.Fatalf("unreadable HEAD selected %q, err=%v", actual, err)
	}
}
