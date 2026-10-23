package steps

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/execpath"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/netretry"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/safeurl"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/shellenv"
	"github.com/kunchenguid/no-mistakes/internal/winproc"
)

func envValue(env []string, key string) (string, bool) {
	return envValueForOS(env, key, runtime.GOOS)
}

func envValueForOS(env []string, key, goos string) (string, bool) {
	prefix := key + "="
	for _, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			return strings.TrimPrefix(entry, prefix), true
		}
		if goos == "windows" && len(entry) >= len(prefix) && strings.EqualFold(entry[:len(prefix)], prefix) {
			return entry[len(prefix):], true
		}
	}
	return "", false
}

func envKey(entry string) string {
	key, _, found := strings.Cut(entry, "=")
	if !found {
		key = entry
	}
	if runtime.GOOS == "windows" {
		return strings.ToUpper(key)
	}
	return key
}

func mergeEnv(extra []string) []string {
	if len(extra) == 0 {
		return nil
	}
	merged := make([]string, 0, len(os.Environ())+len(extra))
	overrides := make(map[string]string, len(extra))
	for _, entry := range extra {
		overrides[envKey(entry)] = entry
	}
	for _, entry := range os.Environ() {
		key := envKey(entry)
		if override, ok := overrides[key]; ok {
			merged = append(merged, override)
			delete(overrides, key)
			continue
		}
		merged = append(merged, entry)
	}
	for _, entry := range extra {
		key := envKey(entry)
		if override, ok := overrides[key]; ok {
			merged = append(merged, override)
			delete(overrides, key)
		}
	}
	return merged
}

// executableCandidates returns the file names to look for, in the order the
// platform would try them. It is the SHARED resolver's, so the step's lookup
// and the git package's lookup select the same program.
func executableCandidates(name string, env []string) []string {
	return execpath.Candidates(env, name)
}

func executableCandidatesForOS(goos, name string, env []string) []string {
	return execpath.CandidatesForOS(goos, name, env)
}

// findInCustomPath resolves the program in the environment's PATH, with the
// SHARED resolver, so this lookup and the git package's lookup cannot pick
// different programs.
func findInCustomPath(workDir string, env []string, name string) string {
	path, _, found := execpath.Resolve(env, name, workDir)
	if !found {
		return ""
	}
	return path
}

func pathCandidateUsable(goos, path string, fi os.FileInfo) bool {
	return execpath.UsableForOS(goos, path, fi)
}

func missingFromCustomPath(env []string, name string) string {
	customPath, ok := execpath.Value(env, "PATH")
	if !ok {
		return ""
	}
	missing := filepath.Join(".", executableCandidates(name, env)[0])
	for _, dir := range filepath.SplitList(customPath) {
		if strings.TrimSpace(dir) == "" {
			continue
		}
		return filepath.Join(dir, executableCandidates(name, env)[0])
	}
	return missing
}

// stepCmd creates an exec.Cmd that inherits the StepContext's extra Env, if any.
// When sctx.Env overrides PATH, the binary is resolved from the overridden PATH
// so that tests can inject fake binaries without modifying the process environment.
func stepCmd(sctx *pipeline.StepContext, name string, args ...string) *exec.Cmd {
	return stepCmdContext(sctx, sctx.Ctx, name, args...)
}

func stepCmdContext(sctx *pipeline.StepContext, ctx context.Context, name string, args ...string) *exec.Cmd {
	env := stepEnvironment(sctx)
	lookupEnv := env
	if lookupEnv == nil {
		lookupEnv = os.Environ()
	}
	resolved := name
	missingFromPath := false
	if !hasExecutablePathSeparator(name) {
		if candidate := findInCustomPath(sctx.WorkDir, lookupEnv, name); candidate != "" {
			resolved = candidate
		} else if _, ok := execpath.Value(lookupEnv, "PATH"); ok {
			resolved = missingFromCustomPath(lookupEnv, name)
			missingFromPath = true
		}
	}
	cmd := exec.CommandContext(ctx, resolved, args...)
	cmd.Dir = sctx.WorkDir
	winproc.Harden(cmd)
	cmd.Env = env
	if missingFromPath {
		cmd.Err = &exec.Error{Name: name, Err: exec.ErrNotFound}
	}
	return cmd
}

func stepEnvironment(sctx *pipeline.StepContext) []string {
	var env []string
	if len(sctx.Env) > 0 {
		env = mergeEnv(sctx.Env)
	}
	if sctx.ForgeContext != nil && !sctx.ForgeContext.Environment.Empty() {
		env = sctx.ForgeContext.Environment.Apply(env)
	}
	return env
}

// stepGitRun runs git with the StepContext's environment plus the standard
// non-interactive git overrides. It is like git.Run but respects sctx.Env so
// step-scoped PATH and credential environment stay in effect.
func stepGitRun(sctx *pipeline.StepContext, args ...string) (string, error) {
	out, err := stepGitRunRaw(sctx, args...)
	return strings.TrimSpace(out), err
}

// stepGitRunRaw preserves NUL-delimited paths and porcelain status columns.
func stepGitRunRaw(sctx *pipeline.StepContext, args ...string) (string, error) {
	return stepGitRunInput(sctx, "", args...)
}

// stepGitRunInput is stepGitRunRaw with stdin, for plumbing such as
// `cat-file --batch-check` that reads its object list from input.
func stepGitRunInput(sctx *pipeline.StepContext, input string, args ...string) (string, error) {
	run := func() ([]byte, error) {
		cmd := stepCmd(sctx, "git", args...)
		if input != "" {
			cmd.Stdin = strings.NewReader(input)
		}
		cmd.Env = git.NonInteractiveEnvFrom(cmd.Env, sctx.WorkDir)
		if len(args) > 0 && (args[0] == "commit" || args[0] == "merge" || args[0] == "rebase" || args[0] == "cherry-pick" || args[0] == "am") {
			var err error
			cmd.Env, err = sctx.CommitAttributionEnv(cmd.Env, func(args ...string) (string, error) { return stepGitRun(sctx, args...) })
			if err != nil {
				return nil, err
			}
		}
		out, err := cmd.Output()
		if err != nil {
			stderr := ""
			if ee, ok := err.(*exec.ExitError); ok {
				stderr = strings.TrimSpace(string(ee.Stderr))
			}
			return nil, fmt.Errorf("git %s: %w: %s", safeurl.RedactText(strings.Join(args, " ")), err, safeurl.RedactText(stderr))
		}
		return out, nil
	}
	// A read that talks to the remote retries a transient network failure.
	// Push is never retried here: it re-checks the remote head first.
	if len(args) > 0 && (args[0] == "fetch" || args[0] == "ls-remote") {
		out, err := netretry.Do(sctx.Ctx, netretry.Options{Operation: "git " + args[0]}, func() ([]byte, error) { return run() })
		return string(out), err
	}
	out, err := run()
	return string(out), err
}

func stepGitHeadSHA(sctx *pipeline.StepContext) (string, error) {
	head, err := stepGitRun(sctx, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	if sctx.Run != nil && sctx.Config != nil {
		if err := sctx.ValidateCommitAttribution(sctx.Run.HeadSHA, head, func(args ...string) (string, error) { return stepGitRun(sctx, args...) }); err != nil {
			return "", err
		}
	}
	return head, nil
}

func stepGitPush(sctx *pipeline.StepContext, remote, ref, expectedSHA string, forceWithLease bool) error {
	return stepGitPushSource(sctx, remote, "HEAD", ref, expectedSHA, forceWithLease)
}

// stepGitPushCommit pushes an explicit commit to a remote ref with the
// StepContext's environment, mirroring git.PushCommit's argument assembly. The
// explicit source SHA (rather than HEAD) is what lets a caller publish exactly
// the commit it verified, even if the worktree moves underneath it.
func stepGitPushCommit(sctx *pipeline.StepContext, remote, commitSHA, ref, expectedSHA string, forceWithLease bool) error {
	return stepGitPushSource(sctx, remote, commitSHA, ref, expectedSHA, forceWithLease)
}

// stepGitPushSource publishes one commit, replaying a transient network failure
// only after re-reading the remote ref: a push whose response was lost may
// already have landed, and a lease that moved must refuse rather than become a
// force push.
func stepGitPushSource(sctx *pipeline.StepContext, remote, source, ref, expectedSHA string, forceWithLease bool) error {
	args := []string{"push", remote}
	if forceWithLease {
		if expectedSHA != "" {
			args = append(args, fmt.Sprintf("--force-with-lease=%s:%s", ref, expectedSHA))
		} else {
			args = append(args, "--force-with-lease")
		}
	}
	args = append(args, source+":"+ref)
	run := func(args ...string) (string, error) { return stepGitRun(sctx, args...) }
	attempts := 0
	_, err := netretry.Do(sctx.Ctx, netretry.Options{Operation: "git push"}, func() (struct{}, error) {
		attempts++
		if attempts > 1 {
			landed, err := git.PushLanded(sctx.Ctx, run, remote, ref, source, expectedSHA, forceWithLease)
			if err != nil {
				return struct{}{}, err
			}
			if landed {
				return struct{}{}, nil
			}
		}
		_, err := run(args...)
		return struct{}{}, err
	})
	return err
}

// stepCLIAvailable checks whether the provider CLI binary is available,
// respecting any custom PATH in sctx.Env.
func stepCLIAvailable(sctx *pipeline.StepContext, provider scm.Provider) bool {
	return stepExecutableAvailable(sctx, provider.CLIName())
}

func stepExecutableAvailable(sctx *pipeline.StepContext, name string) bool {
	if name == "" {
		return false
	}
	if hasExecutablePathSeparator(name) {
		candidate := name
		if !filepath.IsAbs(candidate) && sctx.WorkDir != "" {
			candidate = filepath.Join(sctx.WorkDir, candidate)
		}
		fi, err := os.Stat(candidate)
		return err == nil && pathCandidateUsable(runtime.GOOS, candidate, fi)
	}
	env := stepEnvironment(sctx)
	if env == nil {
		env = os.Environ()
	}
	_, hasPath, found := execpath.Resolve(env, name, sctx.WorkDir)
	if hasPath {
		return found
	}
	_, err := exec.LookPath(name)
	return err == nil
}

func hasExecutablePathSeparator(name string) bool {
	return strings.ContainsRune(name, filepath.Separator) || (filepath.Separator != '/' && strings.ContainsRune(name, '/'))
}

// stepAuthConfigured checks whether the provider CLI is authenticated,
// using sctx.Env to resolve the binary and pass environment variables.
func stepAuthConfigured(sctx *pipeline.StepContext, provider scm.Provider) bool {
	args := provider.AuthCheckCommand()
	if len(args) == 0 {
		return false
	}
	cmd := stepCmd(sctx, args[0], args[1:]...)
	return cmd.Run() == nil
}

// runShellCommand executes a shell command and returns stdout+stderr, exit code, and error.
// A non-zero exit code is not treated as an error - only exec failures return error.
func runShellCommand(ctx context.Context, dir, cmdStr string) (string, int, error) {
	return runShellCommandWithEnv(ctx, dir, nil, cmdStr)
}

func runStepShellCommand(sctx *pipeline.StepContext, cmdStr string) (string, int, error) {
	return runShellCommandWithProcessEnv(sctx.Ctx, sctx.WorkDir, stepEnvironment(sctx), cmdStr)
}

func runShellCommandWithEnv(ctx context.Context, dir string, env []string, cmdStr string) (string, int, error) {
	if len(env) > 0 {
		env = mergeEnv(env)
	}
	return runShellCommandWithProcessEnv(ctx, dir, env, cmdStr)
}

func runShellCommandWithProcessEnv(ctx context.Context, dir string, env []string, cmdStr string) (string, int, error) {
	return runShellCommandWithPriority(ctx, dir, env, cmdStr, 0)
}

func runShellCommandWithPriority(ctx context.Context, dir string, env []string, cmdStr string, nice int) (string, int, error) {
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.CommandContext(ctx, "cmd.exe", "/c", cmdStr)
	} else {
		cmd = exec.CommandContext(ctx, "sh", "-c", shellenv.OwnOOMScoreScript(cmdStr))
	}
	if nice != 0 {
		if nice < 0 || nice > 19 || runtime.GOOS == "windows" {
			return "", -1, fmt.Errorf("unsupported command niceness %d on %s", nice, runtime.GOOS)
		}
		if cmd.Err != nil {
			return "", -1, cmd.Err
		}
		// Run the already-resolved shell under nice with the same arguments.
		cmd = exec.CommandContext(ctx, "nice", append([]string{"-n", strconv.Itoa(nice), cmd.Path}, cmd.Args[1:]...)...)
	}
	shellenv.ConfigureCooperativeShellCommand(cmd)
	cmd.Dir = dir
	if env != nil {
		cmd.Env = env
	}
	out, err := shellenv.CombinedOutputShellCommand(cmd)
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return string(out), ee.ExitCode(), nil
		}
		return string(out), -1, fmt.Errorf("run command %q: %w", cmdStr, err)
	}
	return string(out), 0, nil
}
