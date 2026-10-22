package git

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/runenv"
)

type environmentContextKey struct{}

// WithEnvironment freezes an environment overlay into ctx for Git commands
// and any hooks or credential helpers they spawn.
func WithEnvironment(ctx context.Context, overlay runenv.Overlay) context.Context {
	if overlay.Empty() {
		return ctx
	}
	return context.WithValue(ctx, environmentContextKey{}, overlay.Clone())
}

func nonInteractiveEnvForContext(ctx context.Context, dir string) []string {
	base := os.Environ()
	if ctx != nil {
		if overlay, ok := ctx.Value(environmentContextKey{}).(runenv.Overlay); ok {
			base = overlay.Apply(base)
		}
	}
	return NonInteractiveEnvFrom(base, dir)
}

// NonInteractiveEnv returns the environment for a subprocess that may invoke
// git, with git forced into a fully non-interactive mode. It is intended for
// cmd.Env on any subprocess that may run git (our own git calls and the coding
// agents we spawn).
//
// Without these overrides, git operations such as `git rebase --continue` or
// `git commit` open $EDITOR to confirm a commit message, and remote operations
// can block on a credential prompt. In a headless agent subprocess there is no
// TTY, so the editor or prompt hangs until the agent times out. Pointing the
// editors at `true` makes git accept the existing message immediately, and
// GIT_TERMINAL_PROMPT=0 fails fast instead of blocking on credentials. The
// overrides are appended last so they win over any ambient values (exec
// resolves duplicate keys using the last occurrence).
//
// Pass the same directory assigned to cmd.Dir (or "" when it is unset). When
// cmd.Env is left nil, os/exec injects PWD=cmd.Dir automatically; assigning
// cmd.Env disables that, so callers must thread the working directory through
// here to preserve symlinked working-directory paths (for example /tmp vs
// /private/tmp on macOS, which os.Getwd reports differently depending on PWD).
func NonInteractiveEnv(dir string) []string {
	return NonInteractiveEnvFrom(os.Environ(), dir)
}

// NonInteractiveEnvFrom is NonInteractiveEnv applied to an explicit base
// environment. A nil base means the current process environment.
func NonInteractiveEnvFrom(base []string, dir string) []string {
	if base == nil {
		base = os.Environ()
	}
	env := append(append([]string(nil), base...),
		"GIT_EDITOR=true",
		"GIT_SEQUENCE_EDITOR=true",
		"GIT_TERMINAL_PROMPT=0",
		// Read-only commands such as status and rev-parse must not refresh the
		// index as a side effect. Mutating commands still take required locks.
		"GIT_OPTIONAL_LOCKS=0",
	)
	// Mirror os/exec, which only injects PWD when Cmd.Env is nil, skips it on
	// these platforms, and absolutizes Cmd.Dir first (go.dev/issue/50599):
	// POSIX defines PWD as "an absolute pathname of the current working
	// directory". Injecting a relative dir verbatim (for example ".") poisons
	// every descendant that trusts PWD — macOS /bin/sh is bash 3.2, whose pwd
	// builtin reports "." when PWD="." leaks through git receive-pack into a
	// hook, which is how the post-receive hook of issue #269 ended up passing
	// `--gate .`.
	if dir != "" && runtime.GOOS != "windows" && runtime.GOOS != "plan9" {
		if abs, err := filepath.Abs(dir); err == nil {
			env = append(env, "PWD="+abs)
		}
	}
	return env
}

// bareRepositoryEnv prevents inherited discovery and config-file selection from
// redirecting an explicitly named bare operation into the caller's checkout.
// Preserve identity, credentials and explicit scratch-index settings.
//
// It is the same rule stripRepositorySelectionEnv applies to every command; it
// is kept as a name for the bare form's call site.
func bareRepositoryEnv(env []string) []string {
	return stripRepositorySelectionEnv(env)
}

// stripRepositorySelectionEnv removes the environment variables that name a
// repository or a configuration file, so a Git command runs against the
// directory its caller named and nowhere else.
//
// This is the general form of what bareRepositoryEnv does for the explicitly
// named bare form, and it exists because the discovery form was the open half
// of the same hole. A caller that inherits GIT_DIR or GIT_COMMON_DIR - a T3
// lane, a Git hook, an agent harness, or any process that ran inside a
// repository and exported its location - silently redirects every discovery
// based Git command into THAT repository, no matter which directory the command
// was aimed at. The redirected command then reads and writes the wrong
// repository's configuration: a `git config --worktree` write intended for a
// run worktree lands in a registered checkout's shared .git/config, which is
// how core.bare=true got into a working checkout that stopped being a work
// tree (issue #183).
//
// The command line always wins over the environment, so the explicitly named
// bare form (--git-dir=) is unaffected by this; stripping the variables for
// that form too keeps one rule instead of two.
//
// Identity, credentials, and the scratch-index and object-directory settings
// are preserved: they configure HOW a command runs, not WHICH repository it
// runs against.
func stripRepositorySelectionEnv(env []string) []string {
	result := make([]string, 0, len(env))
	for _, entry := range env {
		key, _, _ := strings.Cut(entry, "=")
		switch key {
		case "GIT_DIR", "GIT_COMMON_DIR", "GIT_WORK_TREE", "GIT_CONFIG":
			continue
		}
		result = append(result, entry)
	}
	return result
}
