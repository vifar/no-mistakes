//go:build !windows && !plan9

package execpath

import "golang.org/x/sys/unix"

// executableByCurrentUser reports whether the CURRENT user may run the file.
//
// An execute bit that belongs to somebody else is not an execute bit for us:
// the file is owned by us with mode 0641, the other user's execute bit is
// set, and launching it fails with "permission denied". The file's mode says
// "somebody may run this"; the access check says whether WE may.
func executableByCurrentUser(path string) bool {
	return unix.Access(path, unix.X_OK) == nil
}
