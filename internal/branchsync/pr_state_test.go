package branchsync

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestRemoteMissingConfirmsLivePRState(t *testing.T) {
	for _, tt := range []struct {
		name       string
		live       scm.PRState
		lookupErr  error
		noHost     bool
		stored     string
		wantState  string
		wantSafety string
		wantHint   bool
	}{
		{name: "merged", live: scm.PRStateMerged, stored: "open", wantState: StateMergedRemoteRemoved, wantSafety: "already_retired"},
		{name: "closed", live: scm.PRStateClosed, stored: "open", wantState: StateClosed, wantSafety: "blocked_closed"},
		{name: "open", live: scm.PRStateOpen, stored: "open", wantState: StateRemoteMissing, wantSafety: "blocked_remote_missing"},
		{name: "lookup fails", lookupErr: errors.New("forge unavailable"), stored: "open", wantState: StateRemoteMissing, wantSafety: "blocked_remote_missing", wantHint: true},
		{name: "no host", noHost: true, stored: "open", wantState: StateRemoteMissing, wantSafety: "blocked_remote_missing", wantHint: true},
		{name: "unknown stored state", live: scm.PRStateMerged, wantState: StateMergedRemoteRemoved, wantSafety: "already_retired"},
		{name: "unrecognized live state", live: scm.PRState("INVALID"), stored: "open", wantState: StateRemoteMissing, wantSafety: "blocked_remote_missing", wantHint: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			for _, check := range []bool{true, false} {
				name := "apply"
				if check {
					name = "check"
				}
				t.Run(name, func(t *testing.T) {
					f := newSyncFixture(t)
					if err := f.db.UpdateRunErrorStatus(f.run.ID, types.RunCIMonitorInterruptedReason, types.RunCIMonitorInterrupted); err != nil {
						t.Fatal(err)
					}
					const prURL = "https://github.com/test/repo/pull/42"
					if err := f.db.UpdateRunPRURL(f.run.ID, prURL); err != nil {
						t.Fatal(err)
					}
					if err := f.db.UpdateRunPRState(f.run.ID, tt.stored); err != nil {
						t.Fatal(err)
					}
					mustRun(t, f.local, "push", f.remote, ":refs/heads/feature/sync")
					refsBefore := mustRun(t, f.local, "show-ref")
					runBefore, err := f.db.GetRun(f.run.ID)
					if err != nil {
						t.Fatal(err)
					}
					calls := 0
					f.service.RemoteTimeout = time.Second
					if !tt.noHost {
						f.service.LookupPRState = func(ctx context.Context, repo *db.Repo, run *db.Run) (scm.PRState, error) {
							calls++
							if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > f.service.RemoteTimeout {
								t.Error("PR lookup has no bounded remote deadline")
							}
							if repo.ID != f.repo.ID || run.ID != f.run.ID || ptr(run.PRURL) != prURL {
								t.Errorf("lookup used the wrong repo or PR: %+v, %+v", repo, run)
							}
							return tt.live, tt.lookupErr
						}
					}
					state := f.service.Refresh(f.ctx)
					if !check {
						calls = 0
						state = f.service.Apply(f.ctx)
					}
					if state.State != tt.wantState || state.Safety != tt.wantSafety || state.Changed {
						t.Errorf("state = %#v, want %s / %s without Git mutation", state, tt.wantState, tt.wantSafety)
					}
					if tt.wantHint && (!strings.Contains(state.Error, "PR state could not be confirmed") || state.NextAction == nil || state.NextAction.Command != "no-mistakes axi sync --check") {
						t.Errorf("missing PR confirmation hint: %#v", state)
					}
					if !tt.wantHint && state.NextAction != nil {
						t.Errorf("unexpected next action: %#v", state.NextAction)
					}
					if calls != 1 && !tt.noHost {
						t.Errorf("PR lookup calls = %d, want 1", calls)
					}
					stored, err := f.db.GetRun(f.run.ID)
					if err != nil {
						t.Fatal(err)
					}
					wantStored := tt.stored
					if !check && !tt.wantHint {
						wantStored = strings.ToLower(string(tt.live))
					}
					if ptr(stored.PRState) != wantStored || stored.Status != types.RunCIMonitorInterrupted {
						t.Errorf("stored PR = %q, status = %s; want %q and unchanged terminal status", ptr(stored.PRState), stored.Status, wantStored)
					}
					if (check || tt.wantHint) && !reflect.DeepEqual(stored, runBefore) {
						t.Error("read-only or failed lookup changed run metadata")
					}
					if got := mustRun(t, f.local, "show-ref"); got != refsBefore || mustRun(t, f.local, "rev-parse", "HEAD") != f.old {
						t.Error("missing-remote classification changed Git refs or HEAD")
					}
					if !check && tt.live == scm.PRStateMerged && tt.lookupErr == nil {
						if cached := f.service.InspectCached(f.ctx); cached.PRState != "merged" {
							t.Errorf("later inspection missed persisted PR state: %#v", cached)
						}
					}
				})
			}
		})
	}
}

func TestRemoteMissingPRLookupTimeoutFailsClosed(t *testing.T) {
	f := newSyncFixture(t)
	if err := f.db.UpdateRunPRURL(f.run.ID, "https://github.com/test/repo/pull/42"); err != nil {
		t.Fatal(err)
	}
	f.service.RemoteTimeout = 20 * time.Millisecond
	f.service.lsRemote = func(context.Context, string, string, string) (string, error) { return "", nil }
	f.service.LookupPRState = func(ctx context.Context, _ *db.Repo, _ *db.Run) (scm.PRState, error) {
		<-ctx.Done()
		return "", ctx.Err()
	}
	before, err := f.db.GetRun(f.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	state := f.service.Apply(f.ctx)
	if state.State != StateRemoteMissing || state.Safety != "blocked_remote_missing" || state.NextAction == nil || !strings.Contains(state.Error, "PR state could not be confirmed") {
		t.Fatalf("lookup timeout did not fail closed: %#v", state)
	}
	after, err := f.db.GetRun(f.run.ID)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("lookup timeout changed run metadata: %v", err)
	}
}

func TestRemoteMissingPRPersistenceFailureFailsClosed(t *testing.T) {
	f := newSyncFixture(t)
	if err := f.db.UpdateRunPRURL(f.run.ID, "https://github.com/test/repo/pull/42"); err != nil {
		t.Fatal(err)
	}
	mustRun(t, f.local, "push", f.remote, ":refs/heads/feature/sync")
	readOnly, err := db.OpenReadOnly(filepath.Join(filepath.Dir(f.local), "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer readOnly.Close()
	f.service.DB = readOnly
	f.service.LookupPRState = func(context.Context, *db.Repo, *db.Run) (scm.PRState, error) {
		return scm.PRStateMerged, nil
	}
	state := f.service.Apply(f.ctx)
	if state.State != StateRemoteMissing || state.Safety != "blocked_remote_missing" || state.NextAction == nil || state.Changed {
		t.Fatalf("persistence failure did not fail closed: %#v", state)
	}
	stored, err := f.db.GetRun(f.run.ID)
	if err != nil || ptr(stored.PRState) != "open" {
		t.Fatalf("persistence failure changed PR state: %#v, %v", stored, err)
	}
}

func TestRemoteMissingWithoutPRURLFailsClosed(t *testing.T) {
	f := newSyncFixture(t)
	if err := f.db.UpdateRunErrorStatus(f.run.ID, types.RunCIMonitorInterruptedReason, types.RunCIMonitorInterrupted); err != nil {
		t.Fatal(err)
	}
	if err := f.db.UpdateRunPRState(f.run.ID, "open"); err != nil {
		t.Fatal(err)
	}
	f.service.LookupPRState = func(context.Context, *db.Repo, *db.Run) (scm.PRState, error) {
		t.Fatal("PR lookup must not run without a recorded PR URL")
		return "", nil
	}
	mustRun(t, f.local, "push", f.remote, ":refs/heads/feature/sync")
	state := f.service.Refresh(f.ctx)
	if state.State != StateRemoteMissing || state.Safety != "blocked_remote_missing" || state.Changed {
		t.Fatalf("state = %#v", state)
	}
	if !strings.Contains(state.Error, "PR state could not be confirmed") || state.NextAction == nil || state.NextAction.Command != "no-mistakes axi sync --check" {
		t.Fatalf("missing PR confirmation hint: %#v", state)
	}
	stored, err := f.db.GetRun(f.run.ID)
	if err != nil || stored.PRState == nil || *stored.PRState != "open" {
		t.Fatalf("run changed: %#v, %v", stored, err)
	}
}
