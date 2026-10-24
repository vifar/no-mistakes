package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// TestMain removes its temporary NM_HOME and HOME after m.Run, but a test
// binary that dies first leaves both behind: a `go test -timeout` panic, a
// SIGTERM or SIGKILL from a cancelled pipeline step or daemon restart, or an
// os.Exit inside a test. Two measures bound that leak.

// testDirPrefixes are the os.MkdirTemp patterns TestMain creates under the
// temp base.
var testDirPrefixes = []string{"nm-cli-test-", "nm-cli-home-"}

// staleTestDirAge is the minimum age for removing a leftover test directory.
// Younger directories are left alone because a concurrent run may own them.
const staleTestDirAge = 6 * time.Hour

// sweepStaleTestDirs removes test directories under base that a killed run
// left behind: directories (not symlinks) matching testDirPrefixes, owned by
// the running account, and last modified at least maxAge before now. It
// returns the directories it removed.
func sweepStaleTestDirs(base string, now time.Time, maxAge time.Duration) []string {
	var removed []string
	for _, prefix := range testDirPrefixes {
		matches, err := filepath.Glob(filepath.Join(base, prefix+"*"))
		if err != nil {
			continue
		}
		for _, dir := range matches {
			info, err := os.Lstat(dir)
			if err != nil || !info.IsDir() || !ownedByCurrentUser(info) {
				continue
			}
			if now.Sub(info.ModTime()) < maxAge {
				continue
			}
			if err := removeTestDir(dir); err == nil {
				removed = append(removed, dir)
			}
		}
	}
	return removed
}

// pinGoCaches points GOMODCACHE and GOCACHE at the caches the operator's go
// already uses. It must run before HOME is replaced: by default both caches
// live under HOME, so child go commands (the tests that build the binary)
// would otherwise fill the temporary HOME with a fresh module cache and
// build cache, about 0.7 GB, that a killed run leaks. If go cannot report
// its caches the variables are left alone.
func pinGoCaches() {
	out, err := exec.Command("go", "env", "GOMODCACHE", "GOCACHE").Output()
	if err != nil {
		return
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) != 2 {
		return
	}
	for i, name := range []string{"GOMODCACHE", "GOCACHE"} {
		if value := strings.TrimSpace(lines[i]); value != "" {
			_ = os.Setenv(name, value)
		}
	}
}
