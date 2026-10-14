package gatecontext_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/gatecontext"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/testgit/gitfault"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestIdentityOrdinaryCloneNeverLaunchesTopLevel(t *testing.T) {
	f := newTopologyFixture(t)
	log := gitfault.Install(t, "top-stall")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	got, err := (gatecontext.Inspector{DB: f.d, Paths: f.p}).Inspect(ctx, gatecontext.Request{CWD: f.work})
	calls, readErr := os.ReadFile(log)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if err != nil || got.Nested {
		t.Fatalf("ordinary clone classification: %+v, %v", got, err)
	}
	if ctx.Err() != nil {
		t.Errorf("ordinary clone exhausted caller deadline: %v", ctx.Err())
	}
	if strings.Contains(string(calls), "--show-toplevel") {
		t.Errorf("unnecessary top-level command launched:\n%s", calls)
	}
	if !strings.Contains(string(calls), "--git-common-dir") {
		t.Errorf("common-directory identity was not read:\n%s", calls)
	}
	if lines := strings.Count(strings.TrimSpace(string(calls)), "\n") + 1; lines != 1 {
		t.Errorf("ordinary clone launched %d Git commands:\n%s", lines, calls)
	}
	if !strings.Contains(string(calls), "locale=C/C") {
		t.Errorf("identity queries did not use English locale:\n%s", calls)
	}
}

func TestIdentityFirstQueryContextFailureIsReturned(t *testing.T) {
	for _, tc := range []struct {
		name   string
		expire bool
		want   error
	}{
		{"canceled", false, context.Canceled}, {"deadline", true, context.DeadlineExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gitfault.Install(t, "common-stall")
			var ctx context.Context
			var cancel context.CancelFunc
			if tc.expire {
				ctx, cancel = context.WithTimeout(context.Background(), 100*time.Millisecond)
			} else {
				ctx, cancel = context.WithCancel(context.Background())
				cancel()
			}
			defer cancel()
			got, err := (gatecontext.Inspector{Paths: paths.WithRoot(t.TempDir())}).Inspect(ctx, gatecontext.Request{CWD: t.TempDir()})
			if !errors.Is(err, tc.want) {
				t.Fatalf("first query returned %+v, %v; want %v", got, err, tc.want)
			}
		})
	}
}

func TestIdentityMalformedMetadataFailsClosed(t *testing.T) {
	for _, name := range []string{"gitfile", "empty-dotgit", "missing-head", "invalid-head", "bare-missing-head", "bare-invalid-head", "bare-missing-objects"} {
		t.Run(name, func(t *testing.T) {
			cwd := t.TempDir()
			switch name {
			case "gitfile":
				if err := os.WriteFile(filepath.Join(cwd, ".git"), []byte("gitdir: missing\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			case "empty-dotgit":
				if err := os.Mkdir(filepath.Join(cwd, ".git"), 0o755); err != nil {
					t.Fatal(err)
				}
			default:
				bare := strings.HasPrefix(name, "bare-")
				if bare {
					run(t, "", "git", "init", "--bare", cwd)
				} else {
					run(t, cwd, "git", "init")
				}
				metadata := cwd
				if !bare {
					metadata = filepath.Join(cwd, ".git")
				}
				switch {
				case strings.HasSuffix(name, "missing-head"):
					if err := os.Remove(filepath.Join(metadata, "HEAD")); err != nil {
						t.Fatal(err)
					}
				case strings.HasSuffix(name, "invalid-head"):
					if err := os.WriteFile(filepath.Join(metadata, "HEAD"), []byte("invalid\n"), 0o644); err != nil {
						t.Fatal(err)
					}
				case strings.HasSuffix(name, "missing-objects"):
					if err := os.RemoveAll(filepath.Join(metadata, "objects")); err != nil {
						t.Fatal(err)
					}
				}
			}
			_, err := (gatecontext.Inspector{Paths: paths.WithRoot(t.TempDir())}).Inspect(context.Background(), gatecontext.Request{CWD: cwd})
			if err == nil {
				t.Fatal("malformed repository metadata was admitted")
			}
		})
	}
}

func TestIdentitySkippedNestedMetadataFailsClosed(t *testing.T) {
	for _, name := range []string{"empty-dotgit", "missing-head", "invalid-head", "bare-missing-head", "bare-invalid-head", "bare-missing-objects"} {
		t.Run(name, func(t *testing.T) {
			outer := t.TempDir()
			run(t, outer, "git", "init")
			nested := filepath.Join(outer, "nested")
			if err := os.Mkdir(nested, 0o755); err != nil {
				t.Fatal(err)
			}
			metadata := filepath.Join(nested, ".git")
			if name == "empty-dotgit" {
				if err := os.Mkdir(metadata, 0o755); err != nil {
					t.Fatal(err)
				}
			} else {
				if strings.HasPrefix(name, "bare-") {
					run(t, "", "git", "init", "--bare", nested)
					metadata = nested
				} else {
					run(t, nested, "git", "init")
				}
				if strings.HasSuffix(name, "invalid-head") {
					if err := os.WriteFile(filepath.Join(metadata, "HEAD"), []byte("invalid\n"), 0o644); err != nil {
						t.Fatal(err)
					}
				} else {
					entry := "HEAD"
					if strings.HasSuffix(name, "missing-objects") {
						entry = "objects"
					}
					if err := os.RemoveAll(filepath.Join(metadata, entry)); err != nil {
						t.Fatal(err)
					}
				}
			}
			cwd := filepath.Join(nested, "child")
			if err := os.Mkdir(cwd, 0o755); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("git", "rev-parse", "--path-format=absolute", "--git-common-dir")
			cmd.Dir = cwd
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("real Git did not skip nested metadata: %v\n%s", err, out)
			}
			want, err := filepath.EvalSymlinks(filepath.Join(outer, ".git"))
			if err != nil {
				t.Fatal(err)
			}
			if filepath.Clean(strings.TrimSpace(string(out))) != want {
				t.Fatalf("real Git identity = %q, want enclosing %q", out, want)
			}
			got, err := (gatecontext.Inspector{Paths: paths.WithRoot(t.TempDir())}).Inspect(context.Background(), gatecontext.Request{CWD: cwd})
			if err == nil {
				t.Fatalf("skipped nested metadata was admitted: %+v; Git reported %q", got, out)
			}
		})
	}
}

func TestIdentityIncompleteBareRemnantsFailClosed(t *testing.T) {
	f := newTopologyFixture(t)
	for _, repository := range []bool{false, true} {
		for _, keep := range []string{"HEAD", "objects", "refs", "detached-HEAD"} {
			t.Run(fmt.Sprintf("repository=%t/%s", repository, keep), func(t *testing.T) {
				outer := t.TempDir()
				if repository {
					run(t, outer, "git", "init")
				}
				nested := filepath.Join(outer, "nested")
				run(t, "", "git", "clone", "--bare", f.work, nested)
				if keep == "detached-HEAD" {
					run(t, nested, "git", "update-ref", "--no-deref", "HEAD", "refs/heads/main")
					keep = "HEAD"
				}
				for _, entry := range []string{"HEAD", "objects", "refs"} {
					if entry != keep {
						if err := os.RemoveAll(filepath.Join(nested, entry)); err != nil {
							t.Fatal(err)
						}
					}
				}
				cwd := filepath.Join(nested, "child")
				if err := os.Mkdir(cwd, 0o755); err != nil {
					t.Fatal(err)
				}
				cmd := exec.Command("git", "rev-parse", "--path-format=absolute", "--git-common-dir")
				cmd.Dir = cwd
				out, err := cmd.CombinedOutput()
				if repository {
					want, pathErr := filepath.EvalSymlinks(filepath.Join(outer, ".git"))
					if pathErr != nil || err != nil || strings.TrimSpace(string(out)) != want {
						t.Fatalf("real Git did not discover enclosing repository: %q, %v, %v", out, err, pathErr)
					}
				} else {
					var exit *exec.ExitError
					if !errors.As(err, &exit) || exit.ExitCode() != 128 || strings.TrimSpace(string(out)) != "fatal: not a git repository (or any of the parent directories): .git" {
						t.Fatalf("real Git did not report discovery absence: %q, %v", out, err)
					}
				}
				got, err := (gatecontext.Inspector{Paths: f.p}).Inspect(context.Background(), gatecontext.Request{CWD: cwd})
				if err == nil {
					t.Fatalf("incomplete bare remnant was admitted: %+v", got)
				}
			})
		}
	}
}

func TestIdentityOrdinaryMetadataNamesStillCheckAncestry(t *testing.T) {
	f := activeIdentityFixture(t)
	records, err := f.d.GetActiveRuns()
	if err != nil || len(records) != 1 {
		t.Fatalf("active fixture run: %v, %v", records, err)
	}
	steps, err := f.d.GetStepsByRun(records[0].ID)
	if err != nil || len(steps) != 1 {
		t.Fatalf("active fixture step: %v, %v", steps, err)
	}
	agentPID := 4100
	if err := f.d.SetStepAgentActivity(steps[0].ID, "started", &agentPID); err != nil {
		t.Fatal(err)
	}
	inspector := gatecontext.Inspector{DB: f.d, Paths: f.p, ParentPID: func(pid int) (int, error) {
		if pid == 4300 {
			return agentPID, nil
		}
		return 1, nil
	}}
	for _, repository := range []bool{false, true} {
		for _, kind := range []string{"file", "directory", "ffd", "fdf", "fdd", "dff", "dfd", "ddf"} {
			for _, name := range []string{"HEAD", "objects", "refs", "all"} {
				if len(kind) == 3 && name != "all" {
					continue
				}
				t.Run(fmt.Sprintf("repository=%t/%s/%s", repository, kind, name), func(t *testing.T) {
					outer := t.TempDir()
					if repository {
						run(t, outer, "git", "init")
					}
					cwd := filepath.Join(outer, "docs")
					if err := os.Mkdir(cwd, 0o755); err != nil {
						t.Fatal(err)
					}
					names := []string{name}
					if name == "all" {
						names = []string{"HEAD", "objects", "refs"}
					}
					for index, entry := range names {
						path := filepath.Join(cwd, entry)
						directory := kind == "directory"
						if len(kind) == 3 {
							directory = kind[index] == 'd'
						}
						if directory {
							if err := os.Mkdir(path, 0o755); err != nil {
								t.Fatal(err)
							}
						} else if err := os.WriteFile(path, []byte("ordinary project content\n"), 0o644); err != nil {
							t.Fatal(err)
						}
					}
					for _, tc := range []struct {
						peer, daemon  int
						nested, agent bool
					}{
						{9000, 0, false, false},
						{4300, 0, true, true},
						{4300, agentPID, true, true},
					} {
						got, err := inspector.Inspect(context.Background(), gatecontext.Request{CWD: cwd, PeerPID: tc.peer, DaemonPID: tc.daemon})
						if err != nil || got.Nested != tc.nested || got.AgentDescendant != tc.agent || got.DaemonDescendant != (tc.daemon != 0) {
							t.Fatalf("ordinary metadata names: %+v, %v", got, err)
						}
						if tc.agent && (got.RunID != records[0].ID || got.Phase != types.StepDocument) {
							t.Fatalf("active-step attribution was lost: %+v", got)
						}
					}
				})
			}
		}
	}
}

func TestIdentityManagedAttributionIgnoresOrdinaryMixedNames(t *testing.T) {
	f := activeIdentityFixture(t)
	records, err := f.d.GetActiveRuns()
	if err != nil || len(records) != 1 {
		t.Fatalf("active fixture run: %v, %v", records, err)
	}
	if err := f.d.SetRunWorktreeDir(records[0].ID, f.managed); err != nil {
		t.Fatal(err)
	}
	cwd := filepath.Join(f.managed, "docs")
	if err := os.MkdirAll(filepath.Join(cwd, "objects"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cwd, "HEAD"), []byte("ordinary project content\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := (gatecontext.Inspector{DB: f.d, Paths: f.p}).Inspect(context.Background(), gatecontext.Request{CWD: cwd})
	if err != nil || !got.ManagedGit || !got.Nested || got.RunID != records[0].ID || got.Phase != types.StepDocument {
		t.Fatalf("managed attribution with ordinary mixed names: %+v, %v", got, err)
	}
}

func TestIdentityBareDiscoveryUnderSafeBareRepositoryExplicit(t *testing.T) {
	f := newTopologyFixture(t)
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "safe.bareRepository")
	t.Setenv("GIT_CONFIG_VALUE_0", "explicit")
	for _, tc := range []struct {
		cwd     string
		managed bool
	}{
		{f.gate, true}, {f.origin, false},
	} {
		got, err := (gatecontext.Inspector{DB: f.d, Paths: f.p}).Inspect(context.Background(), gatecontext.Request{CWD: tc.cwd})
		if err != nil || got.ManagedGit != tc.managed || got.Nested != tc.managed {
			t.Fatalf("explicit bare discovery: %+v, %v", got, err)
		}
	}
	if runtime.GOOS != "windows" {
		link := filepath.Join(t.TempDir(), "bare-link")
		if err := os.Symlink(f.gate, link); err != nil {
			t.Fatal(err)
		}
		got, err := (gatecontext.Inspector{DB: f.d, Paths: f.p}).Inspect(context.Background(), gatecontext.Request{CWD: link})
		if err != nil || !got.ManagedGit || !got.Nested {
			t.Fatalf("symlinked explicit bare discovery: %+v, %v", got, err)
		}
	}
}

func TestIdentityValidMetadataTopologies(t *testing.T) {
	f := newTopologyFixture(t)
	linked := filepath.Join(t.TempDir(), "linked")
	run(t, f.work, "git", "worktree", "add", "--detach", linked, "HEAD")
	separate := filepath.Join(t.TempDir(), "separate")
	gitDir := filepath.Join(filepath.Dir(separate), "metadata")
	run(t, "", "git", "init", "--separate-git-dir", gitDir, separate)
	relative := filepath.Join(t.TempDir(), "relative")
	relativeGitDir := filepath.Join(filepath.Dir(relative), "metadata")
	run(t, "", "git", "init", "--separate-git-dir", relativeGitDir, relative)
	if err := os.WriteFile(filepath.Join(relative, ".git"), []byte("gitdir: ../metadata\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(f.work, "nested")
	run(t, "", "git", "init", nested)
	for _, tc := range []struct {
		name, root string
		managed    bool
	}{
		{"ordinary", f.work, false},
		{"linked", linked, false},
		{"separate", separate, false},
		{"relative-gitfile", relative, false},
		{"nested", nested, false},
		{"bare", f.origin, false},
		{"managed-bare", f.gate, true},
		{"managed-linked", f.managed, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			child := filepath.Join(tc.root, "child")
			if err := os.Mkdir(child, 0o755); err != nil {
				t.Fatal(err)
			}
			for _, cwd := range []string{tc.root, child} {
				got, err := (gatecontext.Inspector{DB: f.d, Paths: f.p}).Inspect(context.Background(), gatecontext.Request{CWD: cwd})
				if err != nil || got.ManagedGit != tc.managed || got.Nested != tc.managed {
					t.Fatalf("valid topology: %+v, %v", got, err)
				}
			}
		})
	}
	if runtime.GOOS != "windows" {
		t.Run("symlinked-metadata", func(t *testing.T) {
			root := t.TempDir()
			if err := os.Symlink(gitDir, filepath.Join(root, ".git")); err != nil {
				t.Fatal(err)
			}
			assertAllowed(t, gatecontext.Inspector{Paths: f.p}, root, "symlinked Git directory")
			link := filepath.Join(t.TempDir(), "worktree-link")
			if err := os.Symlink(relative, link); err != nil {
				t.Fatal(err)
			}
			assertAllowed(t, gatecontext.Inspector{Paths: f.p}, link, "symlinked separate worktree")
		})
	}
}

func TestIdentityMetadataFreeDirectoryStillChecksAncestry(t *testing.T) {
	inspector := gatecontext.Inspector{Paths: paths.WithRoot(t.TempDir()), ParentPID: func(pid int) (int, error) {
		if pid == 4300 {
			return 4100, nil
		}
		return 1, nil
	}}
	cwd := t.TempDir()
	ordinary, err := inspector.Inspect(context.Background(), gatecontext.Request{CWD: cwd, PeerPID: 9000, DaemonPID: 4100})
	if err != nil || ordinary.Nested {
		t.Fatalf("metadata-free ordinary peer: %+v, %v", ordinary, err)
	}
	descendant, err := inspector.Inspect(context.Background(), gatecontext.Request{CWD: cwd, PeerPID: 4300, DaemonPID: 4100})
	if err != nil || !descendant.Nested || !descendant.DaemonDescendant {
		t.Fatalf("metadata-free descendant: %+v, %v", descendant, err)
	}
}

func TestIdentityManagedBareRepositoryRemainsNested(t *testing.T) {
	f := newTopologyFixture(t)
	got, err := (gatecontext.Inspector{DB: f.d, Paths: f.p}).Inspect(context.Background(), gatecontext.Request{CWD: f.gate})
	if err != nil || !got.Nested || !got.ManagedGit {
		t.Fatalf("managed bare identity: %+v, %v", got, err)
	}
}

func activeIdentityFixture(t *testing.T) *topologyFixture {
	t.Helper()
	f := newTopologyFixture(t)
	record, err := f.d.InsertRun(f.repoID, "feature", "head", "base")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.d.UpdateRunStatus(record.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	step, err := f.d.InsertStepResult(record.ID, types.StepDocument)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.d.StartStep(step.ID); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestIdentityManagedAttributionFailureRetainsPositiveProof(t *testing.T) {
	f := activeIdentityFixture(t)
	gitfault.Install(t, "top-error")
	t.Setenv("NM_GIT_FAULT_STDERR", "fatal: unexpected worktree failure")
	got, err := (gatecontext.Inspector{DB: f.d, Paths: f.p}).Inspect(context.Background(), gatecontext.Request{CWD: f.managed})
	if err == nil || !got.Nested || !got.ManagedGit {
		t.Fatalf("managed attribution failure lost proof: %+v, %v", got, err)
	}
}

func TestIdentityManagedBareAttributionAbsenceKeepsProof(t *testing.T) {
	f := activeIdentityFixture(t)
	got, err := (gatecontext.Inspector{DB: f.d, Paths: f.p}).Inspect(context.Background(), gatecontext.Request{CWD: f.gate})
	if err != nil || !got.Nested || !got.ManagedGit {
		t.Fatalf("managed bare attribution absence lost proof: %+v, %v", got, err)
	}
}

func TestIdentityUnknownFailuresNeverProveAbsence(t *testing.T) {
	for _, diagnostic := range []string{
		"fatal: arbitrary discovery failure",
		"fatal: detected dubious ownership in repository at '/fixture'",
		"fatal: not a git repository (or any of the parent directories): .git\nwarning: extra diagnostic",
	} {
		t.Run(diagnostic, func(t *testing.T) {
			gitfault.Install(t, "error")
			t.Setenv("NM_GIT_FAULT_STDERR", diagnostic)
			_, err := (gatecontext.Inspector{Paths: paths.WithRoot(t.TempDir())}).Inspect(context.Background(), gatecontext.Request{CWD: t.TempDir()})
			if err == nil {
				t.Fatal("unresolved Git failure was admitted")
			}
		})
	}
	t.Run("missing executable", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		_, err := (gatecontext.Inspector{Paths: paths.WithRoot(t.TempDir())}).Inspect(context.Background(), gatecontext.Request{CWD: t.TempDir()})
		if err == nil {
			t.Fatal("missing Git executable was admitted")
		}
	})
}

func TestIdentityAbsenceWithIncompleteDrainFailsClosed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("escaped Unix process fixture")
	}
	gitfault.Install(t, "escaped")
	gitfault.EscapedHolder(t)
	_, err := (gatecontext.Inspector{Paths: paths.WithRoot(t.TempDir())}).Inspect(context.Background(), gatecontext.Request{CWD: t.TempDir()})
	var exit *exec.ExitError
	if !errors.Is(err, exec.ErrWaitDelay) || !errors.As(err, &exit) || exit.ExitCode() != 128 {
		t.Fatalf("incomplete drain lost command/drain identity: %v", err)
	}
}

func faultEvent(t *testing.T, log, event string) time.Time {
	t.Helper()
	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) > 1 && fields[1] == event {
			nanos, err := strconv.ParseInt(fields[0], 10, 64)
			if err != nil {
				t.Fatal(err)
			}
			return time.Unix(0, nanos)
		}
	}
	t.Fatalf("missing %s event:\n%s", event, data)
	return time.Time{}
}

func TestIdentityCommandsHaveSharedExecutionBudget(t *testing.T) {
	for _, mode := range []string{"common-stall", "shared-budget"} {
		t.Run(mode, func(t *testing.T) {
			f := activeIdentityFixture(t)
			log := gitfault.Install(t, mode)
			ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
			defer cancel()
			got, err := (gatecontext.Inspector{DB: f.d, Paths: f.p}).Inspect(ctx, gatecontext.Request{CWD: f.managed})
			completed := time.Now()
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("identity command not canceled by execution budget: %+v, %v", got, err)
			}
			if ctx.Err() != nil {
				t.Fatalf("classifier consumed caller deadline: %v", ctx.Err())
			}
			event := "common"
			if mode == "shared-budget" {
				event = "top"
				if !got.Nested || !got.ManagedGit {
					t.Fatalf("positive proof lost: %+v", got)
				}
			}
			canceled := completed
			if runtime.GOOS == "windows" {
				var exit *exec.ExitError
				if !errors.As(err, &exit) || exit.Success() {
					t.Fatalf("budget cancellation did not terminate the helper: %v", err)
				}
			} else {
				canceled = faultEvent(t, log, event+"-canceled")
			}
			initial := faultEvent(t, log, "common-start")
			if elapsed := canceled.Sub(initial); elapsed < 9*time.Second || elapsed > 13*time.Second {
				t.Errorf("shared command budget = %v", elapsed)
			}
			if mode == "shared-budget" {
				topStart := faultEvent(t, log, "top-start")
				if elapsed := canceled.Sub(topStart); elapsed > 8*time.Second {
					t.Errorf("second command received a fresh budget: %v", elapsed)
				}
			}
		})
	}
}
