//go:build windows

package cli

import "io/fs"

// The stale-dir sweep is a no-op on Windows because ownership cannot be
// established cheaply; GOMODCACHE/GOCACHE pinning still keeps a leaked HOME small.
func ownedByCurrentUser(fs.FileInfo) bool { return false }
