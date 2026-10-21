package git

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

const sharedConfigBaselineFile = "no-mistakes-shared-config.json"

// SetWorktreeConfig follows CopyLocalUserIdentity's scope policy. In an
// isolated gate it never acquires the shared config lock. Legacy repositories
// fall back only when Git reports that per-worktree scope is unavailable.
func SetWorktreeConfig(ctx context.Context, dir, key, value string) error {
	if _, err := Run(ctx, dir, "config", "--worktree", key, value); err != nil {
		if !isWorktreeConfigWriteUnavailable(err) {
			return err
		}
		_, err = Run(ctx, dir, "config", "--local", key, value)
		return err
	}
	return nil
}

// sharedConfigKeys reads the FILE, not the effective worktree configuration.
// Values are hashed individually; baseline files and errors never disclose URL
// credentials or other config values. --no-includes keeps the file boundary
// explicit; changes to include directives are themselves detected.
func sharedConfigKeys(ctx context.Context, bare string) (map[string]string, error) {
	path := filepath.Join(bare, "config")
	raw, err := RunRaw(ctx, bare, "config", "--file", path, "--no-includes", "--null", "--list")
	if err != nil {
		return nil, fmt.Errorf("read shared config %s: %w", path, err)
	}
	values := map[string]string{}
	for _, entry := range strings.Split(strings.TrimSuffix(string(raw), "\x00"), "\x00") {
		if entry == "" {
			continue
		}
		key, _, _ := strings.Cut(entry, "\n")
		// Git normalizes section and variable names but preserves subsection case.
		values[key] += entry + "\x00"
	}
	result := make(map[string]string, len(values))
	for key, value := range values {
		sum := sha256.Sum256([]byte(value))
		result[key] = hex.EncodeToString(sum[:])
	}
	return result, nil
}

// RecordSharedConfigBaseline runs only at deliberate enrollment/migration,
// after intended gate config writes. It does not repair an operator's disk.
func RecordSharedConfigBaseline(ctx context.Context, bare string) error {
	keys, err := sharedConfigKeys(ctx, bare)
	if err != nil {
		return err
	}
	enabled, err := RunBare(ctx, bare, "config", "--local", "--type=bool", "--get", "--default=false", "extensions.worktreeConfig")
	if err != nil {
		return err
	}
	if enabled == "true" {
		for _, key := range []string{"core.bare", "core.hookspath"} {
			if _, found := keys[key]; found {
				return sharedConfigChanged(bare, key)
			}
		}
	}
	data, err := json.Marshal(keys)
	if err != nil {
		return err
	}
	return writeGateFileAtomic(filepath.Join(bare, sharedConfigBaselineFile), append(data, '\n'), 0o600, ".shared-config-*")
}

func sharedConfigChanged(bare, key string) error {
	return fmt.Errorf("shared gate config %s: unexpected key or value for %s; use git config --worktree for worktree-scoped settings", filepath.Join(bare, "config"), key)
}

// SharedConfigGuard disables Git's shared-config writes for an executing step
// by reserving Git's own config.lock. Explicit --worktree writes remain usable.
// Multiple steps in this daemon share the reservation; a foreign lock is never
// removed. Direct filesystem writes are caught by Check before step success.
type SharedConfigGuard struct {
	bare     string
	closed   bool
	expected map[string]string
}

var sharedConfigLocks = struct {
	sync.Mutex
	reservations map[string]*sharedConfigReservation
}{reservations: map[string]*sharedConfigReservation{}}

type sharedConfigReservation struct {
	refs int
	info os.FileInfo
}

func ProtectSharedConfig(ctx context.Context, bare string) (*SharedConfigGuard, error) {
	canonical, err := filepath.EvalSymlinks(bare)
	if err != nil {
		return nil, err
	}
	canonical, err = filepath.Abs(canonical)
	if err != nil {
		return nil, err
	}
	guard := &SharedConfigGuard{bare: canonical}
	sharedConfigLocks.Lock()
	defer sharedConfigLocks.Unlock()
	reservation := sharedConfigLocks.reservations[canonical]
	if reservation == nil {
		path := filepath.Join(canonical, "config.lock")
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return nil, fmt.Errorf("disable shared config writes at %s: %w", path, err)
		}
		info, statErr := f.Stat()
		closeErr := f.Close()
		if err := errors.Join(statErr, closeErr); err != nil {
			os.Remove(path)
			return nil, err
		}
		reservation = &sharedConfigReservation{info: info}
		sharedConfigLocks.reservations[canonical] = reservation
	}
	reservation.refs++
	if err := guard.Check(ctx); err != nil {
		_ = guard.closeLocked()
		return nil, err
	}
	return guard, nil
}

func (g *SharedConfigGuard) Check(ctx context.Context) error {
	baselinePath := filepath.Join(g.bare, sharedConfigBaselineFile)
	data, err := os.ReadFile(baselinePath)
	if err != nil {
		return fmt.Errorf("read shared config baseline %s: %w", baselinePath, err)
	}
	var expected map[string]string
	if err := json.Unmarshal(data, &expected); err != nil {
		return fmt.Errorf("read shared config baseline %s: %w", baselinePath, err)
	}
	if g.expected == nil {
		g.expected = expected
	}
	expected = g.expected
	if len(expected) == 0 {
		return fmt.Errorf("empty shared config baseline %s", baselinePath)
	}
	actual, err := sharedConfigKeys(ctx, g.bare)
	if err != nil {
		return err
	}
	// Compare both directions so deletion and edits to intended remotes or the
	// worktreeConfig opt-in cannot evade the boundary check.
	names := map[string]bool{}
	for key := range expected {
		names[key] = true
	}
	for key := range actual {
		names[key] = true
	}
	keys := make([]string, 0, len(names))
	for key := range names {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if actual[key] != expected[key] {
			return sharedConfigChanged(g.bare, key)
		}
	}
	return nil
}

func (g *SharedConfigGuard) Close() error {
	sharedConfigLocks.Lock()
	defer sharedConfigLocks.Unlock()
	return g.closeLocked()
}
func (g *SharedConfigGuard) closeLocked() error {
	if g.closed {
		return nil
	}
	g.closed = true
	reservation := sharedConfigLocks.reservations[g.bare]
	reservation.refs--
	if reservation.refs != 0 {
		return nil
	}
	delete(sharedConfigLocks.reservations, g.bare)
	path := filepath.Join(g.bare, "config.lock")
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("shared config protection disappeared at %s: %w", path, err)
	}
	if !os.SameFile(info, reservation.info) {
		return fmt.Errorf("shared config protection replaced at %s; refusing to remove another writer's lock", path)
	}
	err = os.Remove(path)
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("shared config protection disappeared at %s", filepath.Join(g.bare, "config.lock"))
	}
	return err
}
