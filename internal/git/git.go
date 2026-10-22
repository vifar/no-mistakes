package git

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/execpath"
	"github.com/kunchenguid/no-mistakes/internal/safeurl"
	"github.com/kunchenguid/no-mistakes/internal/shellenv"
	"github.com/kunchenguid/no-mistakes/internal/winproc"
)

// EmptyTreeSHA is the well-known SHA of an empty tree in git.
// Used as a base when there is no prior commit to diff against.
const EmptyTreeSHA = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"

// CommandError retains captured, credential-redacted stderr separately from
// its rendered message. Unwrap preserves command, cancellation and drain causes.
type CommandError struct {
	Command string
	Stderr  string
	Err     error
}

func (e *CommandError) Error() string {
	return fmt.Sprintf("git %s: %v: %s", e.Command, e.Err, strings.TrimSpace(e.Stderr))
}

func (e *CommandError) Unwrap() error { return e.Err }

// refuseStops makes the package's own stop REFUSED. It is a test hook: the
// refusal is what a stop of an already-gone process group looks like, and
// the provenance has to be recorded on that path too. The hook exists so the
// production recording path is exercised, rather than a test that builds the
// marker by hand.
var refuseStops atomic.Bool

// errRefusedStop is the refusal a stopped command reports.
var errRefusedStop = errors.New("operation not permitted")

// cancelRefusalPrefix is the standard library's report of a REFUSED stop. It
// wraps the refusal the cancel hook returned, so the text is the only way to
// recognise it: the wrapper type is unexported.
const cancelRefusalPrefix = "exec: canceling Cmd"

// CancelledExitError is the exit of a subprocess that THIS command's context
// stopped.
//
// The provenance is recorded at the execution boundary, where it is known: the
// context stops its own child, and only that stop is recorded. Observing the
// context afterwards would not prove it - a child can die of SIGSEGV on its
// own and then race a shutdown that happened to observe the context a moment
// later.
//
// A caller that must know whether a failure is the cancellation reads this
// marker, never the shape of the error.
//
// The exit is not always an *exec.ExitError. When the context's stop is
// REFUSED - the process group is already gone, or the platform refuses the
// kill - the standard library reports the refusal ("exec: canceling Cmd: ...")
// instead of the child's exit status, and there is no exit status to carry.
// The refusal is still the context's doing, and the failure it leaves is the
// interruption, not an independent one, so the marker carries the refusal
// itself.
type CancelledExitError struct {
	*exec.ExitError
	// Refused is the refusal the context's stop returned, when the command
	// produced no exit status of its own.
	Refused error
}

func (e *CancelledExitError) Unwrap() error {
	if e.ExitError != nil {
		return e.ExitError
	}
	return e.Refused
}

func (e *CancelledExitError) Error() string {
	if e.ExitError != nil {
		return e.ExitError.Error()
	}
	if e.Refused != nil {
		return e.Refused.Error()
	}
	return "command was cancelled"
}

// IsZeroSHA returns true if the SHA is the null/zero ref that git uses for
// new or deleted branches (40 zeros).
func IsZeroSHA(sha string) bool {
	return sha == "0000000000000000000000000000000000000000"
}

// Run executes a git command in the given directory and returns trimmed stdout.
// Returns an error that includes the command and stderr on failure.
//
// When dir is itself a bare repository (a gate repo), the repo is named
// explicitly via --git-dir instead of relying on cwd-based discovery, which
// safe.bareRepository=explicit forbids. Agent harnesses (e.g. Claude Code)
// and hardened CI inject that setting, so gate operations must never depend
// on discovering a bare repo from the working directory (issue #362).
func Run(ctx context.Context, dir string, args ...string) (string, error) {
	out, err := RunRaw(ctx, dir, args...)
	return strings.TrimSpace(string(out)), err
}

// RunRaw executes a git command and returns stdout without modifying its bytes.
func RunRaw(ctx context.Context, dir string, args ...string) ([]byte, error) {
	if isBareGitDir(dir) {
		return runInDirWithEnvRaw(ctx, dir, nil, append([]string{"--git-dir=" + dir}, args...)...)
	}
	return runInDirWithEnvRaw(ctx, dir, nil, args...)
}

// RunAllowingFailure executes a git command and returns its trimmed stdout even
// when the command exits nonzero.
//
// Some git plumbing reports its result on stdout and signals the result through
// the exit code: `git merge-tree --write-tree` prints the written tree and the
// conflicted paths on stdout and exits 1 when the merge conflicts. Run discards
// stdout on failure, so a caller that needs that answer has to read it here.
// The error is still returned, so the caller can tell a reported conflict from
// a command that failed without producing output.
func RunAllowingFailure(ctx context.Context, dir string, args ...string) (string, error) {
	if isBareGitDir(dir) {
		out, err := runInDirWithEnvRawCaptured(ctx, dir, nil, append([]string{"--git-dir=" + dir}, args...)...)
		return strings.TrimSpace(string(out)), err
	}
	out, err := runInDirWithEnvRawCaptured(ctx, dir, nil, args...)
	return strings.TrimSpace(string(out)), err
}

// RunWithInput executes a git command with exact standard input and returns
// trimmed stdout. It carries the same bare-repository handling as Run.
func RunWithInput(ctx context.Context, dir, input string, args ...string) (string, error) {
	if isBareGitDir(dir) {
		return runInDirWithEnvAndInput(ctx, dir, nil, input, append([]string{"--git-dir=" + dir}, args...)...)
	}
	return runInDirWithEnvAndInput(ctx, dir, nil, input, args...)
}

// RunBare executes Git against exactly bareDir. Unlike Run, it never falls
// back to cwd-based repository discovery when bareDir is malformed. Gate
// recovery uses this after structural validation so an invalid directory under
// NM_HOME cannot discover or mutate an ancestor worktree.
func RunBare(ctx context.Context, bareDir string, args ...string) (string, error) {
	if bareDir == "" {
		return "", fmt.Errorf("bare git directory is empty")
	}
	return runInDir(ctx, bareDir, append([]string{"--git-dir=" + bareDir}, args...)...)
}

// RunWithEnv is Run with extra KEY=VALUE entries appended to the git
// environment. Later entries win, so a caller can override anything
// NonInteractiveEnv sets. It exists for plumbing that is configured only
// through the environment - GIT_INDEX_FILE for a scratch index, the
// GIT_AUTHOR_*/GIT_COMMITTER_* identity for commit-tree - and carries the same
// bare-repository handling as Run.
func RunWithEnv(ctx context.Context, dir string, extraEnv []string, args ...string) (string, error) {
	if isBareGitDir(dir) {
		return runInDirWithEnv(ctx, dir, extraEnv, append([]string{"--git-dir=" + dir}, args...)...)
	}
	return runInDirWithEnv(ctx, dir, extraEnv, args...)
}

// RunDiscoveryWithEnv leaves bare-directory selection to the discovery caller.
// Unlike RunWithEnv, it never injects --git-dir from the general bare heuristic:
// ordinary project entries must not override corroborated identity discovery.
func RunDiscoveryWithEnv(ctx context.Context, dir string, extraEnv []string, args ...string) (string, error) {
	out, err := runInDirWithEnvRaw(ctx, dir, extraEnv, args...)
	return string(out), err
}

func runInDir(ctx context.Context, dir string, args ...string) (string, error) {
	return runInDirWithEnv(ctx, dir, nil, args...)
}

func runInDirWithEnv(ctx context.Context, dir string, extraEnv []string, args ...string) (string, error) {
	return runInDirWithEnvAndInput(ctx, dir, extraEnv, "", args...)
}

func runInDirWithEnvAndInput(ctx context.Context, dir string, extraEnv []string, input string, args ...string) (string, error) {
	out, err := runInDirWithEnvAndInputRaw(ctx, dir, extraEnv, input, args...)
	return strings.TrimSpace(string(out)), err
}

func runInDirWithEnvRaw(ctx context.Context, dir string, extraEnv []string, args ...string) ([]byte, error) {
	return runInDirWithEnvAndInputRaw(ctx, dir, extraEnv, "", args...)
}

func runInDirWithEnvRawCaptured(ctx context.Context, dir string, extraEnv []string, args ...string) ([]byte, error) {
	return runInDirWithEnvAndInputRawCaptured(ctx, dir, extraEnv, "", args...)
}

func runInDirWithEnvAndInputRaw(ctx context.Context, dir string, extraEnv []string, input string, args ...string) ([]byte, error) {
	out, err := runInDirWithEnvAndInputRawCaptured(ctx, dir, extraEnv, input, args...)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// gitBinaryForEnv uses execpath.Resolve so binary lookup sees the same
// environment as the child. Passing a bare name to exec.CommandContext would
// resolve it against the process's PATH before the child's environment is set.
func gitBinaryForEnv(workDir string, env []string) (string, bool) {
	path, hasPath, found := execpath.Resolve(env, "git", workDir)
	if !hasPath {
		return "git", true
	}
	if !found {
		return "", false
	}
	return path, true
}

// runInDirWithEnvAndInputRawCaptured runs a git command and keeps its stdout
// even when the command fails, so a caller can read a result git reports on
// stdout and signals through the exit code. The plain raw runner drops stdout
// on failure, because most callers treat a failed command as no output at all.
func runInDirWithEnvAndInputRawCaptured(ctx context.Context, dir string, extraEnv []string, input string, args ...string) ([]byte, error) {
	env := append(nonInteractiveEnvForContext(ctx, dir), extraEnv...)
	binary, found := gitBinaryForEnv(dir, env)
	cmd := exec.CommandContext(ctx, binary, args...)
	if !found {
		cmd.Err = &exec.Error{Name: "git", Err: exec.ErrNotFound}
	}
	cmd.Dir = dir
	cmd.Env = env
	if len(args) > 0 && strings.HasPrefix(args[0], "--git-dir=") {
		cmd.Env = bareRepositoryEnv(cmd.Env)
	}
	if input != "" {
		cmd.Stdin = strings.NewReader(input)
	}
	winproc.Harden(cmd)
	// OutputShellCommand captures stdout only, so unlike cmd.Output it never
	// fills ExitError.Stderr. Capture stderr explicitly or the git error text
	// below silently becomes empty.
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	shellenv.ConfigureShellCommand(cmd)
	// The context stops its own child, and only that stop proves the exit was
	// the cancellation's doing. Observing the context AFTER the command
	// returned would not: a child can die of a signal on its own and then
	// race a shutdown that happened to observe the context a moment later.
	//
	// The cancel hook is INVOKED by the context and by nothing else, so its
	// invocation is the provenance. The kill it performs can still be
	// refused - the process group may already be gone, or the platform may
	// refuse it - and a refused kill does not make the command's failure
	// independent, because the context is what interrupted it.
	var cancelled atomic.Bool
	// refusal records that the stop was REFUSED. The refusal is the
	// standard library's own report, and the shell package's cleanup can
	// replace it with the child's exit status - so the refusal is recorded
	// HERE, in the hook that saw it, and not read back from the error.
	//
	var refusal atomic.Pointer[error]
	cancelCommand := cmd.Cancel
	cmd.Cancel = func() error {
		cancelled.Store(true)
		if refuseStops.Load() {
			// The stop is refused, exactly as a kill of an
			// already-gone process group is refused. The refusal
			// is what the standard library reports when the
			// process is already done, and it is the shape of
			// this failure: the context interrupted the command,
			// and the stop could not reach the child.
			//
			// The context still interrupted the command, so the
			// refusal is still the cancellation's doing.
			recorded := errRefusedStop
			refusal.Store(&recorded)
			return errRefusedStop
		}
		return cancelCommand()
	}
	out, err := shellenv.OutputShellCommand(cmd)
	if err != nil {
		if cancelled.Load() {
			switch exit := err.(type) {
			case *exec.ExitError:
				// The child produced an exit status. It is the
				// cancellation's doing only if the context's stop
				// REACHED it.
				//
				// A REFUSED stop is the context's doing even when the
				// child also produced an exit status: the refusal
				// means the context could not stop the child, and
				// the shell package's cleanup then killed it. The
				// refusal is the provenance, and the exit is the
				// cleanup's.
				//
				// A child that had ALREADY gone when the context
				// reached it was stopped by something else - the
				// shell package's group termination, most often -
				// and its exit is its own. The kill the context
				// attempted found no process to kill, so the
				// provenance says so.
				if recorded := refusal.Load(); recorded != nil {
					err = &CancelledExitError{ExitError: exit, Refused: *recorded}
				} else if processWasCancelled(exit) {
					err = &CancelledExitError{ExitError: exit}
				}
			default:
				// The stop may have been REFUSED, so the standard
				// library reports the refusal instead of an exit
				// status. The refusal is still the context's doing:
				// the context is what interrupted the command, and a
				// refused kill does not make the failure independent.
				//
				// Only THAT refusal is the cancellation's. Any other
				// error is the command's own.
				if isCancelRefusal(err) {
					err = &CancelledExitError{Refused: err}
				}
			}
		}
		if ctx.Err() != nil {
			err = errors.Join(err, ctx.Err())
		}
		return out, &CommandError{
			Command: safeurl.RedactText(strings.Join(args, " ")),
			Stderr:  safeurl.RedactText(stderr.String()),
			Err:     err,
		}
	}
	return out, nil
}

// StablePatchID returns Git's stable patch identity for one file between two
// commits. The file path is part of the diff, so the same-shaped edit to a
// different file cannot prove content preservation.
//
// The pathspec is explicitly literal: a wildcard or leading colon in a real
// file name would otherwise be read as a glob or as pathspec magic and fold a
// sibling file's diff into this file's identity.
func StablePatchID(ctx context.Context, dir, from, to, path string) (string, error) {
	diff, err := RunRaw(ctx, dir, "diff", "--no-ext-diff", "--binary", from, to, "--", ":(literal)"+path)
	if err != nil {
		return "", err
	}
	if len(diff) == 0 {
		return "", nil
	}
	args := []string{"patch-id", "--stable"}
	if isBareGitDir(dir) {
		args = append([]string{"--git-dir=" + dir}, args...)
	}
	out, err := runInDirWithEnvAndInputRaw(ctx, dir, nil, string(diff), args...)
	if err != nil {
		return "", err
	}
	fields := strings.Fields(string(out))
	if len(fields) == 0 {
		return "", fmt.Errorf("git patch-id --stable returned no identity")
	}
	return fields[0], nil
}

// PatchIdentity pairs a stable patch identity with the commit that carries it.
type PatchIdentity struct {
	// PatchID is Git's stable patch identity.
	PatchID string
	// Commit is the commit the identity was measured from, when the diff
	// stream named one. It is empty when the stream carried no commit.
	Commit string
}

// StablePatchIDFromDiff returns Git's stable patch identity for one whole
// diff, across every file it touches.
//
// StablePatchID measures a single file, so its identity is scoped to that
// path. This measures a complete change, the way `git show <commit> | git
// patch-id --stable` does, so one commit's own change can be compared with
// another commit's own change.
//
// A patch identity discards hunk locations and whitespace, so two different
// changes can share one. It is evidence for a human reading a refusal, never
// a proof that one commit carries another's work.
func StablePatchIDFromDiff(ctx context.Context, dir, diff string) (string, error) {
	if strings.TrimSpace(diff) == "" {
		return "", nil
	}
	out, err := runPatchID(ctx, dir, diff)
	if err != nil {
		return "", err
	}
	fields := strings.Fields(string(out))
	if len(fields) == 0 {
		return "", fmt.Errorf("git patch-id --stable returned no identity")
	}
	return fields[0], nil
}

// StablePatchIdentities returns every stable patch identity in a diff stream,
// in stream order, each paired with the commit that carries it.
//
// The stream is a multi-commit diff, the output of `git log --format=commit %H
// -p` for example. Git reports one identity per patch, followed by the commit
// that produced it.
func StablePatchIdentities(ctx context.Context, dir, stream string) ([]PatchIdentity, error) {
	if strings.TrimSpace(stream) == "" {
		return nil, nil
	}
	out, err := runPatchID(ctx, dir, stream)
	if err != nil {
		return nil, err
	}
	var identities []PatchIdentity
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		identity := PatchIdentity{PatchID: fields[0]}
		if len(fields) > 1 {
			identity.Commit = fields[1]
		}
		identities = append(identities, identity)
	}
	return identities, nil
}

// runPatchID feeds a diff to `git patch-id --stable` and returns its raw
// output.
func runPatchID(ctx context.Context, dir, diff string) ([]byte, error) {
	args := []string{"patch-id", "--stable"}
	if isBareGitDir(dir) {
		args = append([]string{"--git-dir=" + dir}, args...)
	}
	return runInDirWithEnvAndInputRaw(ctx, dir, nil, diff, args...)
}

// ValidateBareRepository verifies both the filesystem shape and Git's own bare
// repository classification. The Git query is explicitly scoped with
// --git-dir, so validation itself cannot discover an ancestor repository.
func ValidateBareRepository(ctx context.Context, bareDir string) error {
	if !isBareGitDir(bareDir) {
		return fmt.Errorf("not a structurally valid bare repository: %s", bareDir)
	}
	out, err := RunBare(ctx, bareDir, "rev-parse", "--is-bare-repository")
	if err != nil {
		return fmt.Errorf("validate bare repository: %w", err)
	}
	if strings.TrimSpace(out) != "true" {
		return fmt.Errorf("repository is not bare: %s", bareDir)
	}
	return nil
}

// LooksLikeBareRepository performs the non-mutating structural half of bare
// repository validation. Call ValidateBareRepository before mutating an
// unstamped directory discovered from the filesystem.
func LooksLikeBareRepository(dir string) bool {
	return isBareGitDir(dir)
}

// isBareGitDir reports whether dir is itself a git directory (a bare repo),
// as opposed to a working tree or linked worktree, which carry a .git entry
// and keep using normal discovery. The check mirrors git's own git-dir
// heuristic: a HEAD file plus an objects directory.
func isBareGitDir(dir string) bool {
	if dir == "" {
		return false
	}
	root, err := os.Lstat(dir)
	if err != nil || !root.IsDir() || root.Mode()&os.ModeSymlink != 0 {
		return false
	}
	if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
		return false
	}
	if fi, err := os.Stat(filepath.Join(dir, "HEAD")); err != nil || fi.IsDir() {
		return false
	}
	fi, err := os.Stat(filepath.Join(dir, "objects"))
	return err == nil && fi.IsDir()
}

// InitBare creates a new bare repository or validates an existing one, refusing
// working checkouts and their non-bare Git directories.
func InitBare(ctx context.Context, path string) error {
	// Refuse both the checkout root and its non-bare Git directory before any
	// `init --bare` write. Refreshing a registered gate needs validation, not
	// another initialization that can rewrite core.bare in the wrong scope.
	if _, err := os.Lstat(filepath.Join(path, ".git")); err == nil {
		return fmt.Errorf("refuse bare initialization of working checkout %s", path)
	} else if !os.IsNotExist(err) {
		return err
	}
	if _, err := os.Lstat(filepath.Join(path, "HEAD")); err == nil {
		return ValidateBareRepository(ctx, path)
	} else if !os.IsNotExist(err) {
		return err
	}
	cmd := exec.CommandContext(ctx, "git", "init", "--bare", path)
	cmd.Env = bareRepositoryEnv(nonInteractiveEnvForContext(ctx, ""))
	winproc.Harden(cmd)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git init --bare: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// AddRemote adds a named remote to the repo at dir.
func AddRemote(ctx context.Context, dir, name, url string) error {
	_, err := Run(ctx, dir, "remote", "add", name, url)
	return err
}

// EnsureRemote sets the named remote to url, adding it when absent and
// updating its URL when it already exists. Idempotent, so it is safe to call
// when repairing or re-running an init.
func EnsureRemote(ctx context.Context, dir, name, url string) error {
	if _, err := GetRemoteURL(ctx, dir, name); err == nil {
		_, err := Run(ctx, dir, "remote", "set-url", name, url)
		return err
	}
	return AddRemote(ctx, dir, name, url)
}

// RemoveRemote removes a named remote from the repo at dir.
func RemoveRemote(ctx context.Context, dir, name string) error {
	_, err := Run(ctx, dir, "remote", "remove", name)
	return err
}

// GetRemoteURL returns the URL of a named remote.
func GetRemoteURL(ctx context.Context, dir, name string) (string, error) {
	return Run(ctx, dir, "remote", "get-url", name)
}

// GetConfiguredRemoteURL returns the literal remote URL from git config,
// without applying url.*.insteadOf rewrites.
func GetConfiguredRemoteURL(ctx context.Context, dir, name string) (string, error) {
	return Run(ctx, dir, "config", "--get", "remote."+name+".url")
}

// GetConfiguredRemoteURLs returns every literal URL configured for a remote.
// Callers that require an authoritative source can reject zero or multiple
// values rather than letting git silently select one.
func GetConfiguredRemoteURLs(ctx context.Context, dir, name string) ([]string, error) {
	out, err := Run(ctx, dir, "config", "--null", "--get-all", "remote."+name+".url")
	if err != nil {
		return nil, err
	}
	return strings.Split(strings.TrimSuffix(out, "\x00"), "\x00"), nil
}

// HasRemote reports whether a remote named name is configured in the repo at
// dir, returning an error if the remote list cannot be read.
func HasRemote(ctx context.Context, dir, name string) (bool, error) {
	out, err := Run(ctx, dir, "remote")
	if err != nil {
		return false, err
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == name {
			return true, nil
		}
	}
	return false, nil
}

// FindGitRoot walks up from path to find the git repository root.
// Resolves symlinks for consistency on macOS (e.g. /tmp -> /private/tmp).
func FindGitRoot(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	cmd := exec.Command("git", "rev-parse", "--show-toplevel")
	cmd.Dir = abs
	winproc.Harden(cmd)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("not a git repository: %s", abs)
	}
	root := strings.TrimSpace(string(out))
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		return root, nil
	}
	return resolved, nil
}

// FindMainRepoRoot returns the root of the main working tree for a git
// repository. Three layouts are supported:
//
//  1. A regular repository or a linked worktree: the git common dir is
//     <root>/.git, so the main working tree is filepath.Dir(commonDir).
//  2. An absorbed submodule (including nested .../modules/a/modules/b):
//     the git common dir lives under the superproject's .git/modules/...
//     and is detached from its working tree. Git writes core.worktree
//     when it absorbs a submodule, pointing at the working tree whose
//     remote.origin.url is the submodule's own origin (which is what
//     callers like init and eject need).
//  3. Exotic GIT_DIR layouts without a core.worktree: fall back to
//     `git rev-parse --show-toplevel` from the original path, the same
//     answer FindGitRoot returns.
//
// In every branch the returned path is run through filepath.EvalSymlinks
// when possible so callers can compare it against other symlink-resolved
// paths (notably on macOS, where /tmp and /private/tmp refer to the same
// directory). Symlink resolution failures fall back to the unresolved
// path, matching the historical behavior.
func FindMainRepoRoot(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}

	// Resolve the git common dir.
	commonDirCmd := exec.Command("git", "rev-parse", "--git-common-dir")
	commonDirCmd.Dir = abs
	winproc.Harden(commonDirCmd)
	commonDirOut, err := commonDirCmd.Output()
	if err != nil {
		return "", fmt.Errorf("not a git repository: %s", abs)
	}
	commonDir := strings.TrimSpace(string(commonDirOut))
	if !filepath.IsAbs(commonDir) {
		commonDir = filepath.Join(abs, commonDir)
	}

	// Branch 1: regular repo or linked worktree. Linked worktrees share
	// the main repo's <root>/.git, so the common dir's basename is still
	// ".git" and its parent is the main working tree.
	if filepath.Base(commonDir) == ".git" {
		return resolveMainRoot(filepath.Dir(commonDir))
	}

	// Branch 2: detached git dir (absorbed submodule). Ask the git dir
	// itself for its core.worktree, which git writes when it absorbs a
	// submodule's git dir. The value is typically relative (e.g.
	// "../../../sub"); resolve it against the common dir.
	worktreeCmd := exec.Command("git", "--git-dir", commonDir, "config", "--get", "core.worktree")
	winproc.Harden(worktreeCmd)
	if worktreeOut, err := worktreeCmd.Output(); err == nil {
		worktree := strings.TrimSpace(string(worktreeOut))
		if worktree != "" {
			if !filepath.IsAbs(worktree) {
				worktree = filepath.Join(commonDir, worktree)
			}
			return resolveMainRoot(worktree)
		}
	}

	// Branch 3: exotic GIT_DIR without a usable core.worktree. Defer to
	// `git rev-parse --show-toplevel` from the original path.
	topCmd := exec.Command("git", "rev-parse", "--show-toplevel")
	topCmd.Dir = abs
	winproc.Harden(topCmd)
	topOut, err := topCmd.Output()
	if err != nil {
		return "", fmt.Errorf("not a git repository: %s", abs)
	}
	return resolveMainRoot(strings.TrimSpace(string(topOut)))
}

// resolveMainRoot applies filepath.EvalSymlinks to path, falling back to
// the unresolved path when symlink resolution fails.
func resolveMainRoot(path string) (string, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return path, nil
	}
	return resolved, nil
}

// Diff returns the unified diff between two commits.
func Diff(ctx context.Context, dir, base, head string) (string, error) {
	return Run(ctx, dir, "diff", base+".."+head)
}

// DiffNameOnly returns the list of files changed between base and head.
// Output is split on newlines with empty entries removed.
func DiffNameOnly(ctx context.Context, dir, base, head string) ([]string, error) {
	out, err := Run(ctx, dir, "diff", "--name-only", base+".."+head)
	if err != nil {
		return nil, err
	}
	var files []string
	for _, line := range strings.Split(out, "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			files = append(files, trimmed)
		}
	}
	return files, nil
}

// DiffStat returns the bounded size of the diff between base and head: the
// number of changed files and the net changed lines (insertions + deletions)
// from `git diff --numstat`. Binary files (numstat "-") contribute a changed
// file but no line count. It carries no paths or content - just two counts.
func DiffStat(ctx context.Context, dir, base, head string) (files, lines int, err error) {
	out, err := Run(ctx, dir, "diff", "--numstat", base+".."+head)
	if err != nil {
		return 0, 0, err
	}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		files++
		added, aerr := strconv.Atoi(fields[0])
		deleted, derr := strconv.Atoi(fields[1])
		if aerr == nil {
			lines += added
		}
		if derr == nil {
			lines += deleted
		}
	}
	return files, lines, nil
}

// CommitTime returns the committer timestamp for a SHA in UTC.
func CommitTime(ctx context.Context, dir, sha string) (time.Time, error) {
	out, err := Run(ctx, dir, "show", "-s", "--format=%ct", sha)
	if err != nil {
		return time.Time{}, err
	}
	secs, err := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse commit time %q: %w", out, err)
	}
	return time.Unix(secs, 0).UTC(), nil
}

// CommitAuthorEmail returns the author email for a SHA.
func CommitAuthorEmail(ctx context.Context, dir, sha string) (string, error) {
	return Run(ctx, dir, "show", "-s", "--format=%ae", sha)
}

// DiffHead returns the unified diff between HEAD and the working tree
// (both staged and unstaged changes).
func DiffHead(ctx context.Context, dir string) (string, error) {
	return Run(ctx, dir, "diff", "HEAD")
}

// Log returns oneline log entries between two commits.
func Log(ctx context.Context, dir, base, head string) (string, error) {
	return Run(ctx, dir, "log", "--oneline", base+".."+head)
}

// HeadSHA returns the full SHA of HEAD.
func HeadSHA(ctx context.Context, dir string) (string, error) {
	return Run(ctx, dir, "rev-parse", "HEAD")
}

// CurrentBranch returns the current branch name.
func CurrentBranch(ctx context.Context, dir string) (string, error) {
	return Run(ctx, dir, "rev-parse", "--abbrev-ref", "HEAD")
}

// IsDetachedHEAD reports whether the working tree is in a detached-HEAD state
// (HEAD points at a commit rather than a branch ref). Uses `git symbolic-ref`
// which fails cleanly when HEAD is not a symbolic ref.
func IsDetachedHEAD(ctx context.Context, dir string) (bool, error) {
	cmd := exec.CommandContext(ctx, "git", "symbolic-ref", "-q", "HEAD")
	cmd.Dir = dir
	winproc.Harden(cmd)
	if err := cmd.Run(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			// Exit 1 means HEAD is not a symbolic ref — detached.
			if ee.ExitCode() == 1 {
				return true, nil
			}
		}
		return false, fmt.Errorf("git symbolic-ref: %w", err)
	}
	return false, nil
}

// DefaultBranch queries a remote to determine its default branch name.
// Uses git ls-remote --symref to read the remote's HEAD symref.
// Falls back to "main" if detection fails (e.g. empty remote, unreachable).
func DefaultBranch(ctx context.Context, dir, remote string) string {
	out, err := Run(ctx, dir, "ls-remote", "--symref", remote, "HEAD")
	if err != nil {
		return "main"
	}
	// Output format: "ref: refs/heads/main\tHEAD\n<sha>\tHEAD\n"
	// Fields splits: ["ref:", "refs/heads/main", "HEAD"]
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "ref: refs/heads/") {
			parts := strings.Fields(line)
			if len(parts) >= 2 {
				return strings.TrimPrefix(parts[1], "refs/heads/")
			}
		}
	}
	return "main"
}

// FetchRemoteBranch fetches a single branch into a remote-tracking ref.
// Uses a force-update refspec (+) so non-fast-forward updates (e.g. after
// a force push on the remote) are accepted instead of silently rejected.
func FetchRemoteBranch(ctx context.Context, dir, remote, branch string) error {
	refspec := fmt.Sprintf("+refs/heads/%s:refs/remotes/%s/%s", branch, remote, branch)
	_, err := Run(ctx, dir, "fetch", "--no-tags", remote, refspec)
	return err
}

func FetchRemoteBranchToRef(ctx context.Context, dir, remote, branch, localRef string) error {
	refspec := fmt.Sprintf("+refs/heads/%s:%s", branch, localRef)
	_, err := Run(ctx, dir, "fetch", "--no-tags", remote, refspec)
	return err
}

// FetchRemoteBranchToPrivateRef fetches one branch into a caller-owned private
// ref without touching FETCH_HEAD or ordinary remote-tracking refs.
func FetchRemoteBranchToPrivateRef(ctx context.Context, dir, remote, branch, localRef string) error {
	refspec := fmt.Sprintf("+refs/heads/%s:%s", branch, localRef)
	_, err := Run(ctx, dir, "fetch", "--no-tags", "--no-write-fetch-head", "--refmap=", remote, refspec)
	return err
}

// FetchRemoteRef imports the object named by one exact remote ref without
// touching FETCH_HEAD or any caller-owned ref. The temporary ref is private to
// this operation, allowing the caller to publish the verified object with its
// own create-only or compare-and-swap policy.
func FetchRemoteRef(ctx context.Context, dir, remote, remoteRef, expectedCommit string) error {
	temporaryRef := fmt.Sprintf("refs/no-mistakes/fetch/%d-%d", os.Getpid(), time.Now().UnixNano())
	defer func() {
		_, _ = Run(context.WithoutCancel(ctx), dir, "update-ref", "--no-deref", "-d", temporaryRef)
	}()
	refspec := fmt.Sprintf("+%s:%s", remoteRef, temporaryRef)
	if _, err := Run(ctx, dir, "fetch", "--no-tags", "--no-write-fetch-head", "--refmap=", remote, refspec); err != nil {
		return err
	}
	fetched, err := Run(ctx, dir, "rev-parse", "--verify", temporaryRef+"^{commit}")
	if err != nil {
		return fmt.Errorf("fetched ref %s is not a commit: %w", remoteRef, err)
	}
	if fetched != strings.TrimSpace(expectedCommit) {
		return fmt.Errorf("fetched ref %s moved to %s, expected %s", remoteRef, fetched, expectedCommit)
	}
	return nil
}

// Push pushes HEAD to a remote ref. If forceWithLease is true, it uses an
// explicit expected remote SHA for safe force-push.
func Push(ctx context.Context, dir, remote, ref, expectedSHA string, forceWithLease bool) error {
	return PushWithOptions(ctx, dir, remote, ref, expectedSHA, forceWithLease, nil)
}

// PushCommit pushes one immutable commit object to a remote ref. Unlike Push,
// a concurrent worktree HEAD move cannot change the source selected by git.
func PushCommit(ctx context.Context, dir, remote, commitSHA, ref, expectedSHA string, forceWithLease bool) error {
	return pushSourceWithOptions(ctx, dir, remote, commitSHA, ref, expectedSHA, forceWithLease, nil, false)
}

// PushCommitWithOptions pushes an immutable commit with hook-visible options.
// It keeps proof launch identity attached to the commit sampled before pushing.
func PushCommitWithOptions(ctx context.Context, dir, remote, commitSHA, ref, expectedSHA string, forceWithLease bool, pushOptions []string) error {
	return pushSourceWithOptions(ctx, dir, remote, commitSHA, ref, expectedSHA, forceWithLease, pushOptions, false)
}

// PushCommitWithOptionsSkippingHooks is PushCommitWithOptions for an internal
// control-plane push that must not invoke the repository's pre-push hook. Use
// it only for a local no-mistakes gate trigger; delivery pushes keep the
// repository hook.
func PushCommitWithOptionsSkippingHooks(ctx context.Context, dir, remote, commitSHA, ref, expectedSHA string, forceWithLease bool, pushOptions []string) error {
	return pushSourceWithOptions(ctx, dir, remote, commitSHA, ref, expectedSHA, forceWithLease, pushOptions, true)
}

// PushWithOptions pushes HEAD to a remote with per-push options.
func PushWithOptions(ctx context.Context, dir, remote, ref, expectedSHA string, forceWithLease bool, pushOptions []string) error {
	return pushSourceWithOptions(ctx, dir, remote, "HEAD", ref, expectedSHA, forceWithLease, pushOptions, false)
}

// PushWithOptionsSkippingHooks is PushWithOptions for an internal control-plane
// push that must not invoke the repository's pre-push hook. Use it only for a
// local no-mistakes gate trigger; delivery pushes keep the repository hook.
func PushWithOptionsSkippingHooks(ctx context.Context, dir, remote, ref, expectedSHA string, forceWithLease bool, pushOptions []string) error {
	return pushSourceWithOptions(ctx, dir, remote, "HEAD", ref, expectedSHA, forceWithLease, pushOptions, true)
}

func pushSourceWithOptions(ctx context.Context, dir, remote, source, ref, expectedSHA string, forceWithLease bool, pushOptions []string, skipHooks bool) error {
	// On an up-to-date push, send-pack sends no ref update but still writes the
	// push options and a closing flush; receive-pack exits on the empty update
	// list without reading them, so that write can kill git with SIGPIPE. Git
	// reports up-to-date before any lease check and runs no hook, so skipping
	// the push gives the same result without the race.
	if len(pushOptions) > 0 && remoteRefAt(ctx, dir, remote, ref, source) {
		return nil
	}
	args := []string{"push"}
	if skipHooks {
		args = append(args, "--no-verify")
	}
	for _, option := range pushOptions {
		args = append(args, "-o", option)
	}
	args = append(args, remote)
	if forceWithLease {
		if expectedSHA != "" {
			args = append(args, fmt.Sprintf("--force-with-lease=%s:%s", ref, expectedSHA))
		} else {
			args = append(args, "--force-with-lease")
		}
	}
	args = append(args, source+":"+ref)
	_, err := Run(ctx, dir, args...)
	return err
}

// remoteRefAt reports whether remote's exact ref already points at source's
// commit. Any lookup failure reports false so the caller pushes as usual.
func remoteRefAt(ctx context.Context, dir, remote, ref, source string) bool {
	commit, err := Run(ctx, dir, "rev-parse", "--verify", source+"^{commit}")
	if err != nil {
		return false
	}
	out, err := Run(ctx, dir, "ls-remote", remote, ref)
	if err != nil {
		return false
	}
	for _, line := range strings.Split(out, "\n") {
		if sha, name, ok := strings.Cut(line, "\t"); ok && name == ref {
			return sha == commit
		}
	}
	return false
}

// LsRemote returns the SHA of a ref on a remote. Returns empty string if the ref doesn't exist.
func LsRemote(ctx context.Context, dir, remote, ref string) (string, error) {
	out, err := Run(ctx, dir, "ls-remote", remote, ref)
	if err != nil {
		return "", err
	}
	if out == "" {
		return "", nil
	}
	// Output format: "<sha>\t<ref>"
	parts := strings.Fields(out)
	if len(parts) < 1 {
		return "", nil
	}
	return parts[0], nil
}

// HasUncommittedChanges reports whether the working tree or index differs from HEAD.
// Returns true if any tracked file is modified, staged, or deleted, or if there are
// untracked files. Equivalent to a non-empty `git status --porcelain`.
func HasUncommittedChanges(ctx context.Context, dir string) (bool, error) {
	out, err := Run(ctx, dir, "status", "--porcelain")
	if err != nil {
		return false, err
	}
	return out != "", nil
}

// UntrackedFiles returns each untracked path in git's order. Ignored files
// are not included.
func UntrackedFiles(ctx context.Context, dir string) ([]string, error) {
	out, err := RunRaw(ctx, dir, "status", "--porcelain", "-z", "--untracked-files=all")
	if err != nil {
		return nil, err
	}
	var files []string
	for _, entry := range strings.Split(string(out), "\x00") {
		if len(entry) < 3 {
			continue
		}
		// Porcelain format: XY <path>\0 where XY is a 2-char status code + space.
		if entry[:2] == "??" {
			files = append(files, entry[3:])
		}
	}
	return files, nil
}

// CreateBranch creates a new branch with the given name and switches to it.
// Fails if the branch already exists.
func CreateBranch(ctx context.Context, dir, name string) error {
	_, err := Run(ctx, dir, "checkout", "-b", name)
	return err
}

// CommitAll stages every change in the working tree and creates a single commit
// with the given message. Fails if there are no changes to commit.
func CommitAll(ctx context.Context, dir, message string) error {
	if _, err := Run(ctx, dir, "add", "-A"); err != nil {
		return err
	}
	dirty, err := HasUncommittedChanges(ctx, dir)
	if err != nil {
		return err
	}
	if !dirty {
		return fmt.Errorf("no changes to commit")
	}
	_, err = Run(ctx, dir, "commit", "-m", message)
	return err
}

// CopyLocalUserIdentity copies local user.name and user.email from srcDir into
// dstDir. Missing values in srcDir are ignored.
//
// The write into dstDir uses per-worktree scope (`git config --worktree`) when
// the repository has worktree config enabled. dstDir is typically a linked
// worktree of the shared gate bare repo, where an unscoped `git config --local`
// write lands in the bare's shared config and takes <bare>/config.lock. Two
// runs starting concurrently on different branches of the same repo then race
// on that single lock and one fails with "could not lock config file ...
// config: File exists". Writing per-worktree puts each run's identity in its own
// <bare>/worktrees/<id>/config.worktree, so concurrent startups never contend.
// Older Git without `--worktree` support falls back to `--local`.
func CopyLocalUserIdentity(ctx context.Context, srcDir, dstDir string) error {
	for _, key := range []string{"user.name", "user.email"} {
		value, err := Run(ctx, srcDir, "config", "--local", "--get", "--default", "", key)
		if err != nil {
			return err
		}
		if value == "" {
			continue
		}
		if err := SetWorktreeConfig(ctx, dstDir, key, value); err != nil {
			return err
		}
	}
	return nil
}

// isWorktreeConfigWriteUnavailable reports whether a `git config --worktree`
// write failed because per-worktree config cannot be used on this repo: either
// the installed Git is too old for the flag (isWorktreeConfigUnsupported), or
// the repo has more than one worktree without extensions.worktreeConfig enabled
// ("--worktree cannot be used with multiple working trees unless the config
// extension worktreeConfig is enabled"). Both mean the caller should fall back
// to the shared --local config.
func isWorktreeConfigWriteUnavailable(err error) bool {
	if isWorktreeConfigUnsupported(err) {
		return true
	}
	return strings.Contains(err.Error(), "worktreeConfig")
}

// WorktreeAdd creates a detached worktree at wtPath checked out to the given SHA.
func WorktreeAdd(ctx context.Context, repoDir, wtPath, sha string) error {
	_, err := Run(ctx, repoDir, "worktree", "add", "--detach", wtPath, sha)
	return err
}

// WorktreeRemove removes a worktree at the given path.
func WorktreeRemove(ctx context.Context, repoDir, wtPath string) error {
	_, err := Run(ctx, repoDir, "worktree", "remove", "--force", wtPath)
	return err
}

// ResolveRef returns the commit SHA that ref resolves to via
// `git rev-parse --verify <ref>^{commit}`. Use it to pin an exact commit
// (e.g. the default-branch tip just fetched) before reading a file from it,
// so a shared-ref worktree cannot serve a stale remote-tracking ref. Returns
// an error if the ref does not resolve to a commit.
func ResolveRef(ctx context.Context, dir, ref string) (string, error) {
	out, err := Run(ctx, dir, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("resolve ref %s: %w", ref, err)
	}
	return out, nil
}

// ExitCode returns the process exit code of a failed git command, and whether
// the error carried one.
//
// Some git plumbing reports its result through the exit code alone.
// `git merge-base --is-ancestor` answers 0 for yes and 1 for no, and
// `git merge-tree --write-tree` answers 1 when the merge conflicts. A caller
// that must tell "the command ran and said no" from "the command could not
// run" needs the code, not only the fact that it failed: the first is a
// result and the second establishes nothing.
func ExitCode(err error) (int, bool) {
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return 0, false
	}
	return exitErr.ExitCode(), true
}

func DirectRefTarget(ctx context.Context, dir, ref string) (string, bool, error) {
	target, err := Run(ctx, dir, "symbolic-ref", "--quiet", "--no-recurse", ref)
	if err == nil {
		return "", false, fmt.Errorf("ref %s is symbolic (target %s)", ref, target)
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
		return "", false, err
	}
	out, err := Run(ctx, dir, "for-each-ref", "--format=%(refname) %(objectname) %(symref)", ref)
	if err != nil {
		return "", false, err
	}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || fields[0] != ref {
			continue
		}
		if len(fields) > 2 {
			return "", false, fmt.Errorf("ref %s is symbolic (target %s)", ref, fields[2])
		}
		if len(fields) != 2 {
			return "", false, fmt.Errorf("ref %s has no direct object target", ref)
		}
		return fields[1], true, nil
	}
	return "", false, nil
}

func ExactRefTarget(ctx context.Context, dir, ref string) (string, bool, error) {
	out, err := Run(ctx, dir, "for-each-ref", "--format=%(refname) %(objectname)", ref)
	if err != nil {
		return "", false, err
	}
	for _, line := range strings.Split(out, "\n") {
		name, target, ok := strings.Cut(strings.TrimSpace(line), " ")
		if ok && name == ref {
			return strings.TrimSpace(target), true, nil
		}
	}
	return "", false, nil
}

// RefExists reports whether the given ref resolves to a commit. It uses
// `git rev-parse --verify --quiet` so a missing ref is a clean (nil, false)
// result rather than a loud error.
func RefExists(ctx context.Context, dir, ref string) (bool, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", dir, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
	cmd.Env = nonInteractiveEnvForContext(ctx, dir)
	winproc.Harden(cmd)
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.ExitCode() == 1 {
			return false, nil
		}
		return false, fmt.Errorf("git rev-parse %s: %w", ref, err)
	}
	return true, nil
}

// ShowFile returns the content of path as stored at the given ref (e.g.
// "HEAD", "origin/main", or a SHA) via `git show <ref>:<path>`. A failure
// (e.g. the path is absent at the ref) is returned as the underlying git
// error from Run; callers that need to distinguish "absent" from a real
// failure should check RefExists first or inspect the error text.
func ShowFile(ctx context.Context, dir, ref, path string) (string, error) {
	out, err := Run(ctx, dir, "show", fmt.Sprintf("%s:%s", ref, path))
	if err != nil {
		return "", err
	}
	return out, nil
}
