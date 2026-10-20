package worktreescratch

import (
	"fmt"
	"path"
	"strings"
)

// knownConfigDirs are hidden directories that hold committed project
// configuration rather than tool scratch. A new one is still staged and
// published; every other hidden directory that is new is treated as scratch.
var knownConfigDirs = map[string]bool{
	".github":       true,
	".gitlab":       true,
	".circleci":     true,
	".devcontainer": true,
	".vscode":       true,
	".husky":        true,
	".changeset":    true,
	".storybook":    true,
}

// IsDirName reports whether a directory name marks tool scratch when the
// directory is new: node_modules, or a hidden directory that is not a known
// project-configuration directory.
func IsDirName(name string) bool {
	if name == "node_modules" {
		return true
	}
	return len(name) > 1 && strings.HasPrefix(name, ".") && name != ".." && !knownConfigDirs[name]
}

func MatchesExclude(file string, patterns []string) (string, bool) {
	file = strings.TrimSuffix(file, "/")
	for _, pattern := range patterns {
		if MatchPattern(file, pattern) {
			return pattern, true
		}
	}
	return "", false
}

func TreeHasDirectory(run func(...string) (string, error), tree, dir string) (bool, error) {
	out, err := run("--literal-pathspecs", "ls-tree", "-z", tree, "--", dir)
	if err != nil {
		return false, err
	}
	fields := strings.Fields(strings.SplitN(out, "\t", 2)[0])
	return len(fields) == 3 && fields[1] == "tree", nil
}

// FirstNewComponent returns the first path component of rel that
// marks tool scratch, or "" when rel is not scratch.
func FirstNewComponent(run func(...string) (string, error), headDirs map[string]bool, rel string, dirPath bool) (string, error) {
	parts := strings.Split(strings.TrimSuffix(rel, "/"), "/")
	if !dirPath {
		parts = parts[:len(parts)-1]
	}
	for i, part := range parts {
		if IsDirName(part) {
			dir := strings.Join(parts[:i+1], "/")
			exists, known := headDirs[dir]
			if !known {
				var err error
				exists, err = TreeHasDirectory(run, "HEAD", dir)
				if err != nil {
					return "", err
				}
				headDirs[dir] = exists
			}
			if !exists {
				return dir + "/", nil
			}
		}
	}
	return "", nil
}

// DirtyPaths lists the worktree's dirty paths that are
// NOT recognized tool scratch. A tracked modification or a new non-scratch
// file is a real edit; a new hidden scratch directory (a test harness's live
// database, for example) is not.
//
// The scratch identity is the SAME one the automatic staging uses, so the two
// can never disagree about what belongs in the branch.
func DirtyPaths(run func(...string) (string, error), excludes []string) ([]string, error) {
	// NUL-delimited records: without -z, git quotes and folds unusual names
	// into one line, and two untracked files can share a line - which would
	// hide a real file behind a scratch one.
	out, err := run("status", "--porcelain", "-z", "--untracked-files=all", "--no-renames", "--ignore-submodules=none")
	if err != nil {
		return nil, err
	}
	headDirs := map[string]bool{}
	var paths []string
	for _, entry := range strings.Split(strings.TrimSuffix(out, "\x00"), "\x00") {
		if entry == "" {
			continue
		}
		if len(entry) < 4 || entry[2] != ' ' {
			return nil, fmt.Errorf("invalid git status entry %q", entry)
		}
		file := entry[3:]
		if entry[0] == '?' || entry[0] == 'A' {
			// An untracked path is scratch when its first new component marks
			// scratch, or when it matches a staging exclude.
			scratch, err := FirstNewComponent(run, headDirs, file, false)
			if err != nil {
				return nil, err
			}
			if scratch != "" {
				continue
			}
			if _, excluded := MatchesExclude(file, excludes); excluded {
				continue
			}
		}
		paths = append(paths, file)
	}
	return paths, nil
}

// MatchPattern checks if a file path matches an ignore pattern.
// Patterns follow gitignore-like semantics:
//   - No slash: match against filename only (e.g., "*.generated.go" matches "pkg/foo.generated.go")
//   - Ends with "/**": match any file under that directory (e.g., "vendor/**" matches "vendor/pkg/foo.go")
//   - Otherwise: path.Match against the full path
//
// file is always a git path, which is "/"-separated on every platform, so the
// slash-based path.Match is the matcher rather than filepath.Match: the latter
// splits on os.PathSeparator, so on Windows its "*" would run straight through
// "/" and silently widen "docs/*.md" into the whole docs subtree. The documented
// rule - "*" never crosses a "/" - is therefore the rule on every host.
func MatchPattern(file, pattern string) bool {
	// "vendor/**" → matches anything under "vendor/"
	if strings.HasSuffix(pattern, "/**") {
		prefix := strings.TrimSuffix(pattern, "/**")
		return file == prefix || strings.HasPrefix(file, prefix+"/")
	}
	// No slash in pattern → match against basename only
	if !strings.Contains(pattern, "/") {
		matched, _ := path.Match(pattern, path.Base(file))
		return matched
	}
	// Full path match
	matched, _ := path.Match(pattern, file)
	return matched
}
