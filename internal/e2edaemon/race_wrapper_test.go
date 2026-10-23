//go:build darwin || linux

package e2edaemon

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The loaded-runner contract gives each package 50 minutes while the CI job
// retains its independent 90-minute wall. Inspect the executable boundary,
// rather than the wrapper's source, so a dead assignment cannot satisfy it.
func TestRaceWrapperGivesEachShardTheLoadedRunnerBudget(t *testing.T) {
	result := runRaceWrapper(t, "")
	if result.status != 0 {
		t.Fatalf("wrapper failed: %s", result.output)
	}
	shards := raceWrapperShards(result.calls)
	if len(shards) != 3 {
		t.Fatalf("got %d shard invocations, want 3", len(shards))
	}
	for _, call := range shards {
		fields := strings.Fields(call)
		var budget time.Duration
		for i, field := range fields {
			if field == "-timeout" && i+1 < len(fields) {
				var err error
				budget, err = time.ParseDuration(fields[i+1])
				if err != nil {
					t.Fatalf("invalid Go timeout in %q: %v", call, err)
				}
			}
		}
		if budget != 50*time.Minute || budget >= 90*time.Minute {
			t.Errorf("package budget = %s, want 50m below the 90m job wall: %s", budget, call)
		}
		t.Logf("Go shard invocation: %s", call)
	}
}

// The race suite is the CI job's wall clock. Run as one go test process, its
// cost is the SUM of every package's race time, which outgrew the job's
// bound: the suite was killed at 45 minutes with internal/pipeline/steps and
// fifteen more packages never started, so the job could not report a failing
// test.
//
// The wrapper splits the suite into parallel shards, so the job costs the
// time of its slowest shard. These tests prove the split is COMPLETE and
// BALANCED - a shard that silently ran nothing would make the suite pass
// without ever running a package.
func TestRaceWrapperRunsEveryPackageExactlyOnce(t *testing.T) {
	result := runRaceWrapper(t, "")
	if result.status != 0 {
		t.Fatalf("wrapper failed: %s", result.output)
	}
	shards := raceWrapperShards(result.calls)
	if len(shards) != 3 {
		t.Fatalf("go calls = %q, want one test invocation per shard", result.calls)
	}
	// Every package in the inventory lands in exactly one shard. The
	// inventory is the suite's own, so a package missing from every shard
	// would stop running without a single failure.
	seen := map[string]int{}
	for _, call := range shards {
		for _, pkg := range strings.Fields(call) {
			if !strings.HasPrefix(pkg, "github.com/") {
				continue
			}
			seen[pkg]++
		}
	}
	for pkg, count := range seen {
		if count != 1 {
			t.Errorf("package %s appears in %d shards, want exactly one", pkg, count)
		}
	}
	for _, pkg := range raceWrapperInventory {
		if seen[pkg] != 1 {
			t.Errorf("package %s ran in %d shards, want exactly one", pkg, seen[pkg])
		}
	}
}

// The measured long poles each cost more than the rest of a shard together,
// so two of them in one shard would make that shard the suite's long pole.
func TestRaceWrapperPlacesOneLongPolePerShard(t *testing.T) {
	result := runRaceWrapper(t, "")
	if result.status != 0 {
		t.Fatalf("wrapper failed: %s", result.output)
	}
	shards := raceWrapperShards(result.calls)
	for _, call := range shards {
		poles := 0
		for _, pole := range raceWrapperPoles {
			if raceWrapperHasPackage(call, pole) {
				poles++
			}
		}
		if poles > 1 {
			t.Errorf("shard carries %d long poles: %q", poles, call)
		}
	}
	// Every pole is placed exactly once, across the shards.
	placed := 0
	for _, call := range shards {
		for _, pole := range raceWrapperPoles {
			if raceWrapperHasPackage(call, pole) {
				placed++
			}
		}
	}
	if placed != 3 {
		t.Errorf("long poles placed %d times, want each of the three exactly once", placed)
	}
}

// A shard that fails is the suite's failure, and the wrapper must report it
// rather than passing on the shards that happened to succeed.
func TestRaceWrapperFailsOnAShardFailure(t *testing.T) {
	result := runRaceWrapper(t, "second")
	if result.status != 29 {
		t.Fatalf("wrapper status = %d, want the failing shard's status 29: %s", result.status, result.output)
	}
}

// A package list that fails is fatal: the shards would otherwise run
// whatever they could find and the suite would pass without every package.
func TestRaceWrapperFailsOnAnUnlistableInventory(t *testing.T) {
	result := runRaceWrapper(t, "nolist")
	if result.status != 1 {
		t.Fatalf("wrapper status = %d, want a failed inventory to reject the suite: %s", result.status, result.output)
	}
	if len(raceWrapperShards(result.calls)) != 0 {
		t.Fatalf("a failed inventory launched shards: %q", result.calls)
	}
}

// A listing that reports a failure while STILL printing the healthy packages
// is the same failure. `go list` can list the packages that are fine and
// report the erroneous ones on stderr, so a check on the output alone would
// accept a partial inventory and the suite would report success without
// ever checking the package that failed to list.
func TestRaceWrapperFailsOnAPartialInventory(t *testing.T) {
	result := runRaceWrapper(t, "partiallist")
	if result.status != 1 {
		t.Fatalf("wrapper status = %d, want a partial inventory to reject the suite: %s", result.status, result.output)
	}
	if len(raceWrapperShards(result.calls)) != 0 {
		t.Fatalf("a partial inventory launched shards: %q", result.calls)
	}
}

// raceWrapperPoles are the measured long poles the wrapper must place one
// per shard.
var raceWrapperPoles = []string{
	"github.com/kunchenguid/no-mistakes/internal/daemon",
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps",
	"github.com/kunchenguid/no-mistakes/internal/pipeline",
}

// raceWrapperShards returns the wrapper's go test invocations, leaving out
// the inventory listing.
func raceWrapperShards(calls []string) []string {
	shards := []string{}
	for _, call := range calls {
		fields := strings.Fields(call)
		if len(fields) == 0 || fields[0] != "test" {
			continue
		}
		shards = append(shards, call)
	}
	return shards
}

// raceWrapperHasPackage reports whether a shard names a package exactly. The
// package list is matched as a whole field, so ./internal/pipeline does not
// count as naming ./internal/pipeline/steps.
func raceWrapperHasPackage(call, pkg string) bool {
	for _, field := range strings.Fields(call) {
		if field == pkg {
			return true
		}
	}
	return false
}

type raceWrapperResult struct {
	status int
	output string
	calls  []string
}

// raceWrapperInventory is the fake go list's package inventory. The
// identities are the ones `go list` emits - the full import path - because
// that is the contract the wrapper's own list has. A fixture that answered
// with relative paths would let a mismatch between the two representations
// pass unnoticed: the long poles would never be excluded from the checksum
// placement, and the expensive package would run twice.
var raceWrapperInventory = []string{
	"github.com/kunchenguid/no-mistakes/cmd/fakeagent",
	"github.com/kunchenguid/no-mistakes/internal/agent",
	"github.com/kunchenguid/no-mistakes/internal/cli",
	"github.com/kunchenguid/no-mistakes/internal/daemon",
	"github.com/kunchenguid/no-mistakes/internal/pipeline",
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps",
	"github.com/kunchenguid/no-mistakes/internal/scm/github",
}

// runRaceWrapper runs scripts/race.sh against a fake go that records its
// invocations. failShard names the shard that fails; "nolist" makes the
// inventory unreadable.
func runRaceWrapper(t *testing.T, failShard string) raceWrapperResult {
	t.Helper()
	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	if err := os.Mkdir(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(dir, "calls")
	fakeGo := filepath.Join(binDir, "go")
	// The fake go list answers the inventory the wrapper asks for.
	listPath := filepath.Join(dir, "list")
	listed := ""
	for _, pkg := range raceWrapperInventory {
		listed += pkg + "\n"
	}
	if err := os.WriteFile(listPath, []byte(listed), 0o644); err != nil {
		t.Fatal(err)
	}
	contents := "#!/bin/sh\nprintf '%s\\n' \"$*\" >>\"$NM_RACE_FAKE_GO_LOG\"\n"
	// A listing that reports a failure while still printing the healthy
	// packages: `go list` can do exactly that, and the wrapper must not
	// accept the partial inventory.
	contents += "case \" $* \" in *' list '*) case \"${NM_RACE_FAKE_GO_FAIL_SHARD:-}\" in nolist) exit 31 ;; partiallist) cat \"$NM_RACE_FAKE_GO_LIST\"; exit 31 ;; esac; cat \"$NM_RACE_FAKE_GO_LIST\"; exit 0 ;; esac\n"
	contents += "status=0\n"
	contents += "case \"$NM_RACE_FAKE_GO_FAIL_SHARD\" in second) case \" $* \" in *'github.com/kunchenguid/no-mistakes/internal/pipeline/steps'*) status=29 ;; esac ;; esac\n"
	contents += "exit \"$status\"\n"
	if err := os.WriteFile(fakeGo, []byte(contents), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", "scripts/race.sh")
	cmd.Dir = filepath.Join("..", "..")
	cmd.Env = append(os.Environ(),
		"PATH="+binDir+":"+os.Getenv("PATH"),
		"NM_RACE_FAKE_GO_LOG="+logPath,
		"NM_RACE_FAKE_GO_LIST="+listPath,
		"NM_RACE_FAKE_GO_FAIL_SHARD="+failShard,
	)
	output, err := cmd.CombinedOutput()
	status := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			status = exitErr.ExitCode()
		} else {
			t.Fatalf("run wrapper: %v", err)
		}
	}
	raw, readErr := os.ReadFile(logPath)
	if readErr != nil && !os.IsNotExist(readErr) {
		t.Fatal(readErr)
	}
	calls := []string{}
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(line) != "" {
			calls = append(calls, line)
		}
	}
	return raceWrapperResult{status: status, output: string(output), calls: calls}
}
