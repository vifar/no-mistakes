//go:build windows

package cli

import "io/fs"

// ownedByCurrentUser is always true on Windows, where TestMain's temp base
// is the per-user os.TempDir.
func ownedByCurrentUser(fs.FileInfo) bool { return true }
