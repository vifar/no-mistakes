package git

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

const sharedConfigBaselineFile = "no-mistakes-shared-config.json"

// commonConfigFile returns the path of the SHARED configuration file of the
// repository that contains dir.
//
// A linked worktree shares its repository's configuration with every other
// worktree of the same repository, so the shared file is the common
// directory's "config", never a per-worktree file. Reading that one path is
// how a write is checked for landing where it was meant to land.
func commonConfigFile(ctx context.Context, dir string) (string, error) {
	out, err := Run(ctx, dir, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return "", fmt.Errorf("resolve the shared configuration file of %s: %w", dir, err)
	}
	common := strings.TrimSpace(out)
	if common == "" {
		return "", fmt.Errorf("resolve the shared configuration file of %s: empty git common directory", dir)
	}
	return filepath.Join(common, "config"), nil
}

// sharedConfigValue reads one key from the SHARED configuration file of the
// repository that contains dir. The read names the file explicitly, so it
// answers for that file alone and not for the effective configuration.
func sharedConfigValue(ctx context.Context, dir, key string) (string, bool, error) {
	path, err := commonConfigFile(ctx, dir)
	if err != nil {
		return "", false, err
	}
	// The internal runner is used deliberately: it does not inject --git-dir,
	// so --file names the one file this read is about.
	raw, err := runInDirWithEnvRaw(ctx, dir, nil, "config", "--file", path, "--no-includes", "--get", key)
	if err != nil {
		if configKeyAbsent(err) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("read %s from %s: %w", key, path, err)
	}
	return strings.TrimSpace(string(raw)), true, nil
}

// configKeyAbsent reports whether a configuration read failed because the key
// is not set, which Git answers with exit status 1.
func configKeyAbsent(err error) bool {
	var exitErr *exec.ExitError
	return errors.As(err, &exitErr) && exitErr.ExitCode() == 1
}

// worktreeConfigEnabled reports whether the repository that contains dir has
// the worktreeConfig extension enabled.
//
// It is the precondition for per-worktree scope to mean anything. Without the
// extension, `git config --worktree` does not fail: in a repository with a
// single worktree it writes to the SHARED configuration instead, and in a
// repository with several it refuses outright. Either way the shared
// configuration is the repository's, and a write there is inherited by every
// other worktree of that repository - which is how a registered checkout ends
// up with core.bare=true (issue #183).
func worktreeConfigEnabled(ctx context.Context, dir string) (bool, error) {
	path, err := commonConfigFile(ctx, dir)
	if err != nil {
		return false, err
	}
	// Let Git interpret every valid boolean spelling in the shared file.
	raw, err := runInDirWithEnvRaw(ctx, dir, nil, "config", "--file", path, "--no-includes", "--type=bool", "--get", "--default=false", "extensions.worktreeConfig")
	if err != nil {
		return false, fmt.Errorf("read extensions.worktreeConfig from %s: %w", path, err)
	}
	return strings.TrimSpace(string(raw)) == "true", nil
}

// isManagedGateRepository reports whether the repository that contains dir is
// a gate no-mistakes owns.
//
// Only such a repository's shared configuration may be written by no-mistakes.
// A gate is a bare repository: no-mistakes created it, its shared
// configuration is no-mistakes' own state, and it is guarded separately (see
// ProtectSharedConfig). The shared configuration of every other repository -
// a working checkout, and every worktree of one - belongs to whoever owns
// that repository.
//
// The answer is Git's own, not a path shape: a working checkout's Git
// directory has the same file layout as a bare repository, so only Git can
// tell them apart.
func isManagedGateRepository(ctx context.Context, dir string) bool {
	out, err := Run(ctx, dir, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return false
	}
	common := strings.TrimSpace(out)
	if common == "" {
		return false
	}
	return ValidateBareRepository(ctx, common) == nil
}

// SetWorktreeConfig writes one key into the repository's PER-WORKTREE
// configuration.
//
// The write is per-worktree or it is refused. The shared configuration of a
// repository belongs to whoever owns that repository, and a shared write is
// inherited by every other worktree of it: a `git config --worktree` write
// that lands in the shared file of a registered checkout is how that checkout
// came to carry core.bare=true and stop being a work tree (issue #183).
//
// A gate no-mistakes owns is the one exception, and only for a Git too old to
// support per-worktree configuration at all. A gate's shared configuration is
// no-mistakes' own state, and it is guarded separately (see ProtectSharedConfig).
func SetWorktreeConfig(ctx context.Context, dir, key, value string) error {
	enabled, err := worktreeConfigEnabled(ctx, dir)
	if err != nil {
		return err
	}
	if enabled {
		if _, err := Run(ctx, dir, "config", "--worktree", key, value); err != nil {
			if !isWorktreeConfigUnsupported(err) {
				return err
			}
			// The installed Git does not know per-worktree scope. Fall back
			// only for a gate no-mistakes owns.
			return setSharedConfigForGate(ctx, dir, key, value)
		}
		return nil
	}
	// Per-worktree scope is not available here. `git config --worktree` would
	// write the shared configuration instead - silently, in a repository with
	// a single worktree - so the write is only allowed for a gate.
	return setSharedConfigForGate(ctx, dir, key, value)
}

// setSharedConfigForGate writes one key into the SHARED configuration of a
// gate no-mistakes owns, and refuses for every other repository.
func setSharedConfigForGate(ctx context.Context, dir, key, value string) error {
	if !isManagedGateRepository(ctx, dir) {
		return fmt.Errorf("per-worktree configuration is unavailable for %s: refusing to write %s into the shared configuration of a repository no-mistakes does not own", dir, key)
	}
	if _, err := Run(ctx, dir, "config", "--local", key, value); err != nil {
		return err
	}
	return nil
}

// sharedConfigKeys reads the FILE, not the effective worktree configuration.
// Values and subsection-bearing key identities are hashed individually;
// baseline files and errors never disclose URL credentials or config values.
// --no-includes keeps the file boundary explicit; changes to include
// directives are themselves detected.
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
		result[sharedConfigKeyIdentity(key)] = hex.EncodeToString(sum[:])
	}
	return result, nil
}

// Subsection names are arbitrary strings: URL rewrites, HTTP/credential
// scopes and even remote names can contain secrets. Preserve ordinary keys for
// actionable diagnostics, but never persist or print subsection-bearing names.
func sharedConfigKeyIdentity(key string) string {
	if strings.Count(key, ".") < 2 {
		return key
	}
	sum := sha256.Sum256([]byte(key))
	return "sha256:" + hex.EncodeToString(sum[:])
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
	// Also redact identities read from a legacy baseline.
	key = sharedConfigKeyIdentity(key)
	if strings.HasPrefix(key, "sha256:") {
		key = "redacted config key (" + key + ")"
	}
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
