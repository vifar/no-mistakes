//go:build !windows

package cli

import (
	"io/fs"
	"os"
	"syscall"
)

// ownedByCurrentUser reports whether info belongs to the running account,
// so a sweep of the shared /tmp never removes another user's directories.
func ownedByCurrentUser(info fs.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(stat.Uid) == os.Getuid()
}
