// Package gatecontext owns the authoritative classification of callers that
// are executing inside an active no-mistakes validation step.
package gatecontext

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"github.com/kunchenguid/no-mistakes/internal/worktrees"
)

const ErrorCode = "nested_gate_context"

// This budget belongs only to identity Git commands. Filesystem discovery,
// database queries and authenticated process ancestry keep their own owners.
const identityCommandBudget = 10 * time.Second

var identityLocale = []string{"LC_ALL=C", "LANG=C"}

// RefusalMessage is the stable, privacy-safe error contract used at daemon
// ingress and non-AXI command boundaries.
func RefusalMessage(result Result) string {
	target := "an active no-mistakes validation step"
	if result.RunID != "" && result.Phase != "" {
		target = fmt.Sprintf("no-mistakes run %s, phase %s", result.RunID, result.Phase)
	} else if result.RunID != "" {
		target = "no-mistakes run " + result.RunID
	}
	return fmt.Sprintf("%s: refusing pipeline control from %s; return control to the outer executor", ErrorCode, target)
}

// Request contains the independently observable signals for one caller.
// MarkerPresent is diagnostic only. SkipManagedGit is reserved for managed Git
// hooks, whose cwd is necessarily the gate itself and therefore cannot identify
// the process that initiated the push.
type Request struct {
	CWD            string
	PeerPID        int
	DaemonPID      int
	MarkerPresent  bool
	SkipManagedGit bool
}

// Result is safe to render to an agent. It intentionally carries no paths or
// process details.
type Result struct {
	Nested           bool
	ManagedGit       bool
	AgentDescendant  bool
	DaemonDescendant bool
	MarkerPresent    bool
	RunID            string
	Phase            types.StepName
}

// Inspector combines canonical managed Git identity with authenticated IPC
// peer ancestry. ParentPID exists for deterministic tests; production callers
// leave it nil to use the platform process table.
type Inspector struct {
	DB        *db.DB
	Paths     *paths.Paths
	ParentPID func(int) (int, error)
	// ActiveAgentPIDs supplies live executor ownership for daemon cancellation,
	// whose authorization must remain available when database reads fail.
	ActiveAgentPIDs func() []int
}

// Inspect classifies a caller without mutating repositories, runs, refs,
// remotes, worktrees, or the database.
func (i Inspector) Inspect(ctx context.Context, req Request) (Result, error) {
	result := Result{MarkerPresent: req.MarkerPresent}
	if i.Paths == nil {
		return result, fmt.Errorf("gate execution context: paths are required")
	}

	var managedRepoID string
	var commonDir string
	identityCtx, cancelIdentity := context.WithTimeout(ctx, identityCommandBudget)
	defer cancelIdentity()
	if !req.SkipManagedGit && strings.TrimSpace(req.CWD) != "" {
		var err error
		commonDir, err = gitCommonDirectory(identityCtx, req.CWD)
		if err != nil {
			return result, err
		}
		if commonDir != "" {
			id, managed, err := i.registeredManagedCommonDir(commonDir)
			if err != nil {
				return result, err
			}
			if managed {
				managedRepoID = id
				result.ManagedGit = true
				result.Nested = true
			}
		}
	}

	active, err := i.activeAgentSteps()
	if err != nil {
		return result, err
	}
	if req.PeerPID > 0 && (len(active) > 0 || req.DaemonPID > 0) {
		parent := i.ParentPID
		if parent == nil {
			parent = processParentPID
		}
		chain, err := ancestry(req.PeerPID, parent)
		if err != nil {
			return result, fmt.Errorf("gate execution context: inspect authenticated peer ancestry: %w", err)
		}
		if req.DaemonPID > 0 && req.PeerPID != req.DaemonPID && chain[req.DaemonPID] {
			result.Nested = true
			result.DaemonDescendant = true
		}
		var matches []activeAgentStep
		for _, step := range active {
			if step.agentPID > 0 && chain[step.agentPID] {
				matches = append(matches, step)
			}
		}
		if len(matches) > 0 {
			result.Nested = true
			result.AgentDescendant = true
		}
		// A persistent adapter server can be shared by concurrent steps. Name
		// the enclosing run only when authenticated ancestry identifies exactly
		// one active phase; refusal itself does not depend on this metadata.
		if len(matches) == 1 {
			result.RunID = matches[0].runID
			result.Phase = matches[0].phase
		}
	}

	// Canonical topology remains authoritative when a marker is removed or an
	// adapter omits it. Attach run/phase only when the worktree path and active
	// DB record agree exactly; otherwise omit them rather than guessing.
	if result.ManagedGit && result.RunID == "" && managedRepoID != "" {
		var worktreeRoot string
		// Where a run's worktree is is recorded on the run, so the comparison
		// reads that placement rather than deriving one from configuration this
		// process may not be able to read (see worktrees.RecordedDir).
		for _, step := range active {
			if step.repoID != managedRepoID {
				continue
			}
			if worktreeRoot == "" {
				top, err := runIdentityGit(identityCtx, req.CWD, "rev-parse", "--show-toplevel")
				if err != nil {
					// A registered bare gate has no worktree root. This known absence
					// costs attribution only; positive managed identity remains decisive.
					if sameCanonicalPath(req.CWD, commonDir) && normalGitDiagnostic(err, "fatal: this operation must be run in a work tree") {
						break
					}
					return result, fmt.Errorf("gate execution context: attribute managed worktree: %w", err)
				}
				if top == "" || !filepath.IsAbs(top) {
					return result, fmt.Errorf("gate execution context: managed worktree has no absolute top-level identity")
				}
				worktreeRoot = top
			}
			want := worktrees.RecordedDir(i.Paths, step.worktreeDir, step.repoID, step.runID)
			if sameCanonicalPath(worktreeRoot, want) {
				result.RunID = step.runID
				result.Phase = step.phase
				break
			}
		}
	}
	return result, nil
}

type activeAgentStep struct {
	runID  string
	repoID string
	// worktreeDir is the run's recorded worktree placement, empty for a run
	// recorded before it was durable. It travels with the step so attribution
	// compares against the directory the run was created in rather than the
	// one current configuration would derive.
	worktreeDir string
	phase       types.StepName
	agentPID    int
}

func (i Inspector) activeAgentSteps() ([]activeAgentStep, error) {
	if i.ActiveAgentPIDs != nil {
		var out []activeAgentStep
		for _, pid := range i.ActiveAgentPIDs() {
			out = append(out, activeAgentStep{agentPID: pid})
		}
		return out, nil
	}
	if i.DB == nil {
		return nil, nil
	}
	// The narrow placement query rather than whole run rows: this runs in the
	// CLI preflight, which reads a database no command has migrated yet, so it
	// must not name a column the schema may not have (see
	// db.ActiveRunWorktrees).
	runs, err := i.DB.ActiveRunWorktrees()
	if err != nil {
		return nil, fmt.Errorf("gate execution context: list active runs: %w", err)
	}
	var out []activeAgentStep
	for _, run := range runs {
		steps, err := i.DB.GetStepsByRun(run.RunID)
		if err != nil {
			return nil, fmt.Errorf("gate execution context: list steps for active run: %w", err)
		}
		for _, step := range steps {
			if !activeStepStatus(step.Status) {
				continue
			}
			pid := 0
			if step.AgentPID != nil {
				pid = *step.AgentPID
			}
			out = append(out, activeAgentStep{runID: run.RunID, repoID: run.RepoID, worktreeDir: run.Dir, phase: step.StepName, agentPID: pid})
		}
	}
	return out, nil
}

func activeStepStatus(status types.StepStatus) bool {
	switch status {
	case types.StepStatusRunning, types.StepStatusFixing, types.StepStatusAwaitingApproval, types.StepStatusFixReview:
		return true
	default:
		return false
	}
}

func (i Inspector) registeredManagedCommonDir(commonDir string) (string, bool, error) {
	common := canonicalPath(commonDir)
	reposDir := canonicalPath(i.Paths.ReposDir())
	if filepath.Dir(common) != reposDir {
		return "", false, nil
	}
	base := filepath.Base(common)
	if !strings.HasSuffix(base, ".git") || base == ".git" || !git.LooksLikeBareRepository(common) {
		return "", false, nil
	}
	id := strings.TrimSuffix(base, ".git")
	if i.DB == nil {
		return "", false, nil
	}
	repo, err := i.DB.GetRepo(id)
	if err != nil {
		return "", false, fmt.Errorf("gate execution context: verify managed gate: %w", err)
	}
	if repo == nil || !sameCanonicalPath(common, i.Paths.RepoDir(repo.ID)) {
		return "", false, nil
	}
	return repo.ID, true, nil
}

func gitCommonDirectory(ctx context.Context, cwd string) (string, error) {
	identity, err := runIdentityGit(ctx, cwd, "rev-parse", "--path-format=absolute", "--git-common-dir", "--absolute-git-dir")
	if err != nil {
		if normalGitDiagnostic(err, "fatal: not a git repository (or any of the parent directories): .git") {
			absent, fsErr := gitMetadataMatchesDiscovery(cwd, "")
			if fsErr != nil {
				return "", fmt.Errorf("gate execution context: corroborate Git discovery absence: %w: %w", err, fsErr)
			}
			if absent {
				return "", nil
			}
		}
		return "", fmt.Errorf("gate execution context: discover Git common directory: %w", err)
	}
	dirs := strings.Split(identity, "\n")
	if len(dirs) != 2 || !filepath.IsAbs(dirs[0]) || !filepath.IsAbs(dirs[1]) {
		return "", fmt.Errorf("gate execution context: Git returned no absolute directory identities")
	}
	matched, err := gitMetadataMatchesDiscovery(cwd, dirs[1])
	if err != nil {
		return "", fmt.Errorf("gate execution context: corroborate Git discovery identity: %w", err)
	}
	if !matched {
		return "", fmt.Errorf("gate execution context: Git discovery skipped repository metadata")
	}
	return dirs[0], nil
}

func runIdentityGit(ctx context.Context, cwd string, args ...string) (string, error) {
	physical, err := filepath.Abs(cwd)
	if err != nil {
		return "", err
	}
	physical, err = filepath.EvalSymlinks(physical)
	if err != nil {
		return "", err
	}
	if _, err := os.Lstat(filepath.Join(physical, ".git")); err != nil {
		if !os.IsNotExist(err) {
			return "", err
		}
		if git.LooksLikeBareRepository(physical) {
			bare, err := bareGitMetadataPresent(physical)
			if err != nil {
				return "", err
			}
			if bare {
				args = append([]string{"--git-dir=" + physical}, args...)
			}
		}
	}
	return git.RunDiscoveryWithEnv(ctx, cwd, identityLocale, args...)
}

// Only a sole normal exit can prove a discovery absence. A joined output-drain
// failure or a context wrapper is deliberately ineligible, even with exit 128.
func normalGitDiagnostic(err error, diagnostic string) bool {
	var commandErr *git.CommandError
	if !errors.As(err, &commandErr) {
		return false
	}
	exitErr, ok := commandErr.Unwrap().(*exec.ExitError)
	return ok && exitErr.ProcessState.Exited() && exitErr.ExitCode() == 128 &&
		strings.TrimSpace(commandErr.Stderr) == diagnostic
}

func gitMetadataMatchesDiscovery(cwd, gitDir string) (bool, error) {
	absolute, err := filepath.Abs(cwd)
	if err != nil {
		return false, err
	}
	physical, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return false, err
	}
	info, err := os.Stat(physical)
	if err != nil {
		return false, err
	}
	if !info.IsDir() {
		return false, fmt.Errorf("caller directory is not a directory")
	}
	if gitDir != "" {
		gitDir, err = filepath.EvalSymlinks(gitDir)
		if err != nil {
			return false, err
		}
	}
	for dir := physical; ; dir = filepath.Dir(dir) {
		if dir == gitDir {
			return true, nil
		}
		entry := filepath.Join(dir, ".git")
		if _, err := os.Lstat(entry); err == nil {
			if gitDir == "" {
				return false, nil
			}
			info, err := os.Stat(entry)
			if err != nil {
				return false, err
			}
			if info.Mode().IsRegular() {
				data, err := os.ReadFile(entry)
				if err != nil {
					return false, err
				}
				target, ok := strings.CutPrefix(strings.TrimRight(string(data), "\r\n"), "gitdir: ")
				if !ok || target == "" {
					return false, nil
				}
				if !filepath.IsAbs(target) {
					target = filepath.Join(dir, target)
				}
				entry = target
			} else if !info.IsDir() {
				return false, nil
			}
			resolved, err := filepath.EvalSymlinks(entry)
			return resolved == gitDir, err
		} else if !os.IsNotExist(err) {
			return false, err
		}
		bare, err := bareGitMetadataPresent(dir)
		if err != nil {
			return false, err
		}
		if bare {
			return false, nil
		}
		if filepath.Dir(dir) == dir {
			return gitDir == "", nil
		}
	}
}

func bareGitMetadataPresent(dir string) (bool, error) {
	if info, err := os.Stat(filepath.Join(dir, "HEAD")); err == nil {
		if info.Mode().IsRegular() {
			file, err := os.Open(filepath.Join(dir, "HEAD"))
			if err != nil {
				return false, err
			}
			data, err := io.ReadAll(io.LimitReader(file, 128))
			_ = file.Close()
			if err != nil {
				return false, err
			}
			head := strings.TrimSpace(string(data))
			if strings.HasPrefix(head, "ref: ") && strings.TrimSpace(strings.TrimPrefix(head, "ref: ")) != "" {
				return true, nil
			}
			if len(head) == 40 || len(head) == 64 {
				if _, err := hex.DecodeString(head); err == nil {
					return true, nil
				}
			}
		}
	} else if !os.IsNotExist(err) {
		return false, err
	}
	for _, structure := range [][]string{
		{"objects", "objects/info"},
		{"objects", "objects/pack"},
		{"refs", "refs/heads"},
		{"refs", "refs/tags"},
	} {
		present := true
		for _, entry := range structure {
			info, err := os.Stat(filepath.Join(dir, filepath.FromSlash(entry)))
			if err != nil {
				if !os.IsNotExist(err) {
					return false, err
				}
				present = false
				break
			}
			if !info.IsDir() {
				present = false
				break
			}
		}
		if present {
			return true, nil
		}
	}
	return false, nil
}

func ancestry(pid int, parent func(int) (int, error)) (map[int]bool, error) {
	chain := make(map[int]bool)
	for depth := 0; pid > 0 && depth < 256; depth++ {
		if chain[pid] {
			break
		}
		chain[pid] = true
		if pid == 1 {
			break
		}
		next, err := parent(pid)
		if err != nil {
			return nil, err
		}
		pid = next
	}
	return chain, nil
}

// canonicalPath is the shared worktree_roots canonicalization, so a path that
// compares equal here compares equal everywhere placement is resolved.
func canonicalPath(path string) string {
	return worktrees.Canonical(path)
}

func sameCanonicalPath(a, b string) bool {
	return canonicalPath(a) == canonicalPath(b)
}

// MarkerPresent reports the coarse diagnostic marker emitted by adapters.
// Classification never rejects on this signal alone.
func MarkerPresent() bool {
	_, ok := os.LookupEnv(agent.GateRoleEnvVar)
	return ok
}
