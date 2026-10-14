package gatecontext_test

import (
	"context"
	"errors"
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
