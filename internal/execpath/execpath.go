// Package execpath resolves an executable from the PATH in an environment.
//
// It exists because a command's environment and the command's BINARY are two
// different things, and a resolver that reads one while the child sees the
// other picks a different program than the caller asked for.
//
// os/exec keeps the LAST entry for a repeated key, so the environment the
// child receives is not always the one a first-match reader would describe.
// A resolver therefore has to read the same way the child does.
package execpath

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Value returns the value of key in env, the way the child's environment will
// see it: os/exec keeps the LAST entry for a repeated key, so a reader that
// took the first would look in a different place than the child does.
//
// The key match is case-insensitive on Windows, where the environment's keys
// are case-insensitive.
func Value(env []string, key string) (string, bool) {
	return ValueForOS(env, key, runtime.GOOS)
}

// ValueForOS is Value for an explicit platform.
func ValueForOS(env []string, key, goos string) (string, bool) {
	prefix := key + "="
	value := ""
	found := false
	for _, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			value = strings.TrimPrefix(entry, prefix)
			found = true
			continue
		}
		if goos == "windows" && len(entry) >= len(prefix) && strings.EqualFold(entry[:len(prefix)], prefix) {
			value = entry[len(prefix):]
			found = true
		}
	}
	return value, found
}

// Candidates returns the file names to look for, in the order the platform
// would try them. On Windows a name without an extension is also tried with
// each extension in PATHEXT, so the lookup selects the same program the
// platform's own lookup would.
func Candidates(env []string, name string) []string {
	return CandidatesForOS(runtime.GOOS, name, env)
}

// CandidatesForOS is Candidates for an explicit platform.
func CandidatesForOS(goos, name string, env []string) []string {
	candidates := []string{name}
	if goos != "windows" || filepath.Ext(name) != "" {
		return candidates
	}
	pathExt := ".COM;.EXE;.BAT;.CMD"
	if customPathExt, ok := ValueForOS(env, "PATHEXT", goos); ok {
		pathExt = customPathExt
	}
	for _, ext := range strings.Split(pathExt, ";") {
		ext = strings.TrimSpace(ext)
		if ext == "" {
			continue
		}
		candidates = append(candidates, name+ext)
	}
	return candidates
}

// Usable reports whether a candidate is a program the platform would run.
func Usable(path string, fi os.FileInfo) bool {
	return UsableForOS(runtime.GOOS, path, fi)
}

// UsableForOS is Usable for an explicit platform.
func UsableForOS(goos, path string, fi os.FileInfo) bool {
	if fi.IsDir() {
		return false
	}
	if goos == "windows" {
		return filepath.Ext(path) != ""
	}
	return fi.Mode().Perm()&0o111 != 0
}

// Resolve returns the program named name in the environment's PATH.
//
// The second result says whether the environment NAMED a PATH at all. An
// environment that names none leaves the process's PATH in charge, which is
// what every caller that does not choose one expects. An environment that
// names an EMPTY PATH names no program at all: the child receives an empty
// PATH, so running the process's binary would be running a program the
// caller did not choose.
//
// The third result says whether a program was found. When the environment
// named a PATH and it holds no such program, the caller is told so rather
// than handed a different binary.
//
// A relative PATH entry is resolved against baseDir, because that is the
// directory the command runs in and the directory the child will see.
func Resolve(env []string, name string, baseDir string) (path string, hasPath bool, found bool) {
	customPath, ok := Value(env, "PATH")
	if !ok {
		return "", false, false
	}
	candidates := Candidates(env, name)
	for _, dir := range filepath.SplitList(customPath) {
		if dir == "" {
			continue
		}
		if !filepath.IsAbs(dir) {
			dir = filepath.Join(baseDir, dir)
		}
		for _, candidateName := range candidates {
			candidate := filepath.Join(dir, candidateName)
			if fi, err := os.Stat(candidate); err == nil && Usable(candidate, fi) {
				return candidate, true, true
			}
		}
	}
	return "", true, false
}
