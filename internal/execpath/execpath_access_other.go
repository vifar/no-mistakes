//go:build windows || plan9

package execpath

// executableByCurrentUser reports whether the CURRENT user may run the file.
//
// These platforms do not report a POSIX wait status or a POSIX access mode,
// so the file's own extension is the whole of the check.
func executableByCurrentUser(path string) bool {
	return true
}
