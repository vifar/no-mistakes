//go:build darwin || linux

package e2edaemon

import (
	"bufio"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The 15 terminal-merge scenarios added by #95 must not queue behind the
// remaining journeys: at 30fb0ae1 that serial tail exhausted the shared 2400s
// budget in two CI runs. Execute the wrapper and inspect its child invocations.
func TestE2EWrapperTerminalMergeJourneyHasIndependentWorker(t *testing.T) {
	result := runE2EWrapper(t, "", "")
	if result.status != 0 {
		t.Fatalf("wrapper failed: %s", result.output)
	}
	terminal := callContaining(t, result.calls, "-run ^(TestTargetAdvanceTerminalMergeLiveJourney|TestRetainedTargetContinuityLiveJourney)$")
	remainder := callContaining(t, result.calls, "-skip ^(TestUserJourney|TestTargetAdvanceTerminalMergeLiveJourney|TestRetainedTargetContinuityLiveJourney)$")
	if !strings.Contains(terminal, "-timeout ") || !strings.Contains(remainder, "-timeout ") {
		t.Fatalf("workers must retain the shared suite budget: %q", result.calls)
	}
}

// Regression at f5fcfcb2: CI's terminal worker passed in1013.254s while
// the remainder exhausted2255s. The added nine-case table took923.27s in
// measured local validation, so place it beside the terminal table.
func TestE2EWrapperBalancesRetainedTargetTable(t *testing.T) {
	result := runE2EWrapper(t, "", "")
	if result.status != 0 {
		t.Fatalf("wrapper failed: %s", result.output)
	}
	callContaining(t, result.calls, "-run ^(TestTargetAdvanceTerminalMergeLiveJourney|TestRetainedTargetContinuityLiveJourney)$")
	callContaining(t, result.calls, "-skip ^(TestUserJourney|TestTargetAdvanceTerminalMergeLiveJourney|TestRetainedTargetContinuityLiveJourney)$")
}

func TestE2EWrapperDefaultPartitionsJourneyAndRunsRemainder(t *testing.T) {
	result := runE2EWrapperWithCap(t, "", "", "08")
	if result.status != 0 {
		t.Fatalf("wrapper status = %d, output: %s", result.status, result.output)
	}
	calls := result.calls
	// Two matrix processes, two discovery calls, the terminal journey, the
	// pipeline-step worker, one shard per remaining daemon slot, and the
	// cleanup. The shard count is bounded by what the daemon cap leaves -
	// the terminal worker takes one slot - and by the runner's own capacity,
	// so this fixture's cap of 8 yields the maximum of four shards.
	if len(calls) != 11 {
		t.Fatalf("go calls = %q, want matrix, discovery, terminal journey, step tests, four shards, and cleanup", calls)
	}
	assertBothMatrixCallsFinishedBeforeCleanup(t, result.events)
	first := callContaining(t, calls, "-run ^TestUserJourney$/(claude|codex)$")
	second := callContaining(t, calls, "-skip ^TestUserJourney$/(claude|codex)$")
	if !strings.Contains(first, "-run ^TestUserJourney$/(claude|codex)$") || strings.Contains(first, "-skip") {
		t.Errorf("first matrix invocation = %q, want claude/codex selector only", first)
	}
	if !strings.Contains(second, "-run ^TestUserJourney$") || !strings.Contains(second, "-skip ^TestUserJourney$/(claude|codex)$") {
		t.Errorf("second matrix invocation = %q, want complementary selector", second)
	}
	if !strings.Contains(first, "-timeout 2400s") || !strings.Contains(second, "-timeout 2400s") || !strings.Contains(first, "./internal/e2e/...") || !strings.Contains(second, "./internal/e2e/...") {
		t.Errorf("matrix invocations must each receive the full shared upper-bound timeout: %q", calls[:2])
	}
	remaining := callContaining(t, calls, "-skip ^(TestUserJourney|TestTargetAdvanceTerminalMergeLiveJourney|TestRetainedTargetContinuityLiveJourney)$ ./internal/e2e/...")
	if !strings.Contains(remaining, "-timeout ") {
		t.Errorf("remaining-suite timeout missing: %q", remaining)
	}
	// The suite contract is that every discovered test runs exactly once,
	// including tests that exist only in a steps package. Evaluate the actual
	// child selectors against the discovery inventory, independently of the
	// wrapper's placement algorithm.
	for _, name := range strings.Fields(wrapperDiscoveredTests) {
		selected := 0
		for _, call := range calls {
			fields := strings.Fields(call)
			if len(fields) == 0 || fields[0] != "test" || strings.Contains(call, " -list ") {
				continue
			}
			run, skip := ".*", "a^"
			for i := 0; i+1 < len(fields); i++ {
				if fields[i] == "-run" {
					run = fields[i+1]
				}
				if fields[i] == "-skip" {
					skip = fields[i+1]
				}
			}
			// The step-test worker runs the step tests WHOLE: it names no
			// -run selector, because it is not a selection - it is the
			// step-test package. A journey is never run by it, so a
			// journey is not counted against it.
			if run == ".*" && !strings.Contains(call, "./internal/e2e/...") {
				if name == "TestStepOnly" {
					selected++
				}
				continue
			}
			// Matrix selectors address subtests; the parent is their owner.
			run = strings.SplitN(run, "/", 2)[0]
			skip = strings.SplitN(skip, "/", 2)[0]
			if regexp.MustCompile(run).MatchString(name) && !regexp.MustCompile(skip).MatchString(name) {
				selected++
			}
		}
		if name != "TestUserJourney" && selected != 1 {
			t.Errorf("discovered test %s selected %d times, want exactly once", name, selected)
		}
	}
}

// The shards are balanced by MEASURED TIME. A count-based split leaves one
// shard as the suite's long pole while the rest idle, and the suite then
// costs the sum of its shards instead of the time of its slowest one. The
// two shards must be within a small fraction of each other.
func TestE2EWrapperBalancesShardsByMeasuredTime(t *testing.T) {
	result := runE2EWrapperWithCap(t, "", "", "08")
	if result.status != 0 {
		t.Fatalf("wrapper status = %d, output: %s", result.status, result.output)
	}
	costs := e2eJourneyCosts(t)
	loads := []int{}
	for _, call := range result.calls {
		fields := strings.Fields(call)
		if len(fields) == 0 || fields[0] != "test" {
			continue
		}
		run, skip := "", ""
		for i := 0; i+1 < len(fields); i++ {
			if fields[i] == "-run" {
				run = fields[i+1]
			}
			if fields[i] == "-skip" {
				skip = fields[i+1]
			}
		}
		if run == "" || skip == "" {
			// A matrix invocation, not a shard.
			continue
		}
		pattern := regexp.MustCompile(strings.Trim(run, "^$"))
		skipPattern := regexp.MustCompile(strings.Trim(skip, "^$"))
		load := 0
		for name, cost := range costs {
			if pattern.MatchString(name) && !skipPattern.MatchString(name) {
				load += cost
			}
		}
		if load > 0 {
			loads = append(loads, load)
		}
	}
	if len(loads) < 2 {
		t.Fatalf("shards = %v, want at least two shards to compare", loads)
	}
	low, high := loads[0], loads[0]
	for _, load := range loads[1:] {
		if load < low {
			low = load
		}
		if load > high {
			high = load
		}
	}
	// The spread is bounded: a shard may not carry more than a quarter more
	// than the lightest one, or the suite is paying for the imbalance.
	if high > low+low/4 {
		t.Errorf("shard loads = %v, want the spread within a quarter of the lightest shard", loads)
	}
}

// e2eJourneyCosts reads the suite's measured journey costs.
func e2eJourneyCosts(t *testing.T) map[string]int {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "scripts", "e2e-journey-costs.txt"))
	if err != nil {
		t.Fatal(err)
	}
	costs := map[string]int{}
	inList := false
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "journey_seconds=(") {
			inList = true
			continue
		}
		if inList && line == ")" {
			inList = false
			continue
		}
		if !inList {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		seconds, err := strconv.Atoi(fields[0])
		if err != nil {
			t.Fatalf("journey cost %q is not a number: %v", fields[0], err)
		}
		costs[fields[1]] = seconds
	}
	if len(costs) == 0 {
		t.Fatal("the suite read no measured journey costs")
	}
	return costs
}

func TestE2EWrapperWaitsForBothMatrixProcessesAndPropagatesFailure(t *testing.T) {
	result := runE2EWrapper(t, "first", "")
	if result.status != 17 {
		t.Fatalf("wrapper status = %d, want first matrix failure 17; output: %s", result.status, result.output)
	}
	if len(result.calls) != 3 {
		t.Fatalf("go calls = %q, want both matrix processes and cleanup, with no remaining suite", result.calls)
	}
	assertBothMatrixCallsFinishedBeforeCleanup(t, result.events)
	if !hasCallContaining(result.calls, "-run ^TestUserJourney$/(claude|codex)$") || !hasCallContaining(result.calls, "-skip ^TestUserJourney$/(claude|codex)$") {
		t.Fatalf("both matrix invocations must have completed before wrapper exits: %q", result.calls)
	}
}

func TestE2EWrapperPropagatesTerminalJourneyFailure(t *testing.T) {
	result := runE2EWrapper(t, "terminal", "")
	if result.status != 19 {
		t.Fatalf("wrapper status = %d, want terminal failure 19; output: %s", result.status, result.output)
	}
	// The default fixture's daemon cap is 2: the terminal worker takes one
	// slot, so exactly one shard runs beside it. The calls are the two
	// matrix processes, the two discovery calls, the terminal worker, the
	// step-test worker, the shard, and the cleanup.
	if len(result.calls) != 8 || result.events[len(result.events)-1] != "cleanup" {
		t.Fatalf("both remaining workers must finish before cleanup: calls=%q events=%q", result.calls, result.events)
	}
}

func TestE2EWrapperDiscoveryFailureStartsNoWorkers(t *testing.T) {
	result := runE2EWrapper(t, "discovery", "")
	if result.status != 1 {
		t.Fatalf("wrapper status = %d, want failed discovery to reject the suite: %s", result.status, result.output)
	}
	if len(result.calls) != 4 || result.events[len(result.events)-1] != "cleanup" {
		t.Fatalf("discovery failure launched workers or skipped cleanup: calls=%q events=%q", result.calls, result.events)
	}
}

func TestE2EWrapperShardFailureWaitsForWorkersAndFailsTheSuite(t *testing.T) {
	result := runE2EWrapper(t, "shard", "")
	if result.status != 29 {
		t.Fatalf("wrapper status = %d, want shard failure 29: %s", result.status, result.output)
	}
	if len(result.calls) != 8 || result.events[len(result.events)-1] != "cleanup" {
		t.Fatalf("shard failure lost workers or cleanup: calls=%q events=%q", result.calls, result.events)
	}
}

func callContaining(t *testing.T, calls []string, fragment string) string {
	t.Helper()
	for _, call := range calls {
		if strings.Contains(call, fragment) {
			return call
		}
	}
	t.Fatalf("no go invocation contains %q: %q", fragment, calls)
	return ""
}

func hasCallContaining(calls []string, fragment string) bool {
	for _, call := range calls {
		if strings.Contains(call, fragment) {
			return true
		}
	}
	return false
}

func assertBothMatrixCallsFinishedBeforeCleanup(t *testing.T, events []string) {
	t.Helper()
	cleanup := -1
	ended := make([]int, 0, 2)
	for i, event := range events {
		if event == "cleanup" {
			cleanup = i
		}
		if strings.HasPrefix(event, "end:test -tags=e2e -count=1 -timeout 2400s -run ^TestUserJourney$") {
			ended = append(ended, i)
		}
	}
	if len(ended) != 2 || cleanup <= ended[len(ended)-1] {
		t.Fatalf("events = %q, want both matrix processes ended before cleanup", events)
	}
}

func TestE2EWrapperExplicitArgsRemainUnchanged(t *testing.T) {
	result := runE2EWrapper(t, "", "-tags=e2e -run TestSpecific ./internal/e2e/...")
	if result.status != 0 {
		t.Fatalf("wrapper status = %d, output: %s", result.status, result.output)
	}
	if len(result.calls) != 2 || !hasCallContaining(result.calls, "test -tags=e2e -run TestSpecific ./internal/e2e/...") {
		t.Fatalf("go calls = %q, want reaper and unchanged explicit test args", result.calls)
	}
}

func TestE2EWrapperCapOneKeepsSerialDefault(t *testing.T) {
	result := runE2EWrapperWithCap(t, "", "", "1")
	if result.status != 0 {
		t.Fatalf("wrapper status = %d, output: %s", result.status, result.output)
	}
	if len(result.calls) != 2 || !hasCallContaining(result.calls, "test -tags=e2e -count=1 -timeout 2400s ./internal/e2e/... ./internal/pipeline/steps/...") {
		t.Fatalf("go calls = %q, want one serial default suite and cleanup", result.calls)
	}
}

func TestE2EWrapperSignalTerminatesOwnedProcessGroupsBeforeCleanup(t *testing.T) {
	for _, tc := range []struct {
		name           string
		stage          string
		signal         syscall.Signal
		expectedStatus int
	}{
		{name: "term-matrix", stage: "matrix", signal: syscall.SIGTERM, expectedStatus: 143},
		{name: "interrupt-matrix", stage: "matrix", signal: syscall.SIGINT, expectedStatus: 130},
		{name: "term-remaining", stage: "remaining", signal: syscall.SIGTERM, expectedStatus: 143},
		{name: "interrupt-remaining", stage: "remaining", signal: syscall.SIGINT, expectedStatus: 130},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runE2EWrapperSignalCase(t, tc.stage, tc.signal, tc.expectedStatus)
		})
	}
}

func runE2EWrapperSignalCase(t *testing.T, stage string, signal syscall.Signal, expectedStatus int) {
	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	if err := os.Mkdir(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	readyFIFO := filepath.Join(dir, "ready")
	gateFIFO := filepath.Join(dir, "gate")
	if err := syscall.Mkfifo(readyFIFO, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(gateFIFO, 0o600); err != nil {
		t.Fatal(err)
	}
	eventsPath := filepath.Join(dir, "events")
	childPath := filepath.Join(binDir, "fake-child")
	child := "#!/bin/sh\n"
	child += "selector=\"$1\"\n"
	child += "trap 'printf \"child-end:%s\\n\" \"$selector\" >>\"$NM_E2E_FAKE_GO_EVENTS\"; exit 0' TERM INT\n"
	child += "printf 'child-ready:%s:%s:%s\\n' \"$selector\" \"$NM_E2E_FAKE_PARENT_PID\" \"$$\" >\"$NM_E2E_FAKE_GO_READY\"\n"
	child += "while :; do IFS= read -r _ <\"$NM_E2E_FAKE_GO_GATE\"; done\n"
	if err := os.WriteFile(childPath, []byte(child), 0o755); err != nil {
		t.Fatal(err)
	}
	fakeGo := filepath.Join(binDir, "go")
	goScript := "#!/bin/sh\n"
	goScript += "case \" $* \" in *' run ./internal/e2edaemon/reapmain.go '*) printf 'cleanup\\n' >>\"$NM_E2E_FAKE_GO_EVENTS\"; exit 0 ;; esac\n"
	goScript += wrapperDiscoveryScript
	goScript += "case \"$*\" in *' -skip ^(TestUserJourney|TestTargetAdvanceTerminalMergeLiveJourney|TestRetainedTargetContinuityLiveJourney)$ ./internal/e2e/...'*) [ \"$NM_E2E_FAKE_GO_STAGE\" = remaining ] || exit 0; selector=remaining ;; *'./internal/pipeline/steps/...'*) [ \"$NM_E2E_FAKE_GO_STAGE\" = remaining ] || exit 0; printf 'step-tests-done\\n' >>\"$NM_E2E_FAKE_GO_EVENTS\"; exit 0 ;; *' -run ^(TestTargetAdvanceTerminalMergeLiveJourney|TestRetainedTargetContinuityLiveJourney)$ '*) [ \"$NM_E2E_FAKE_GO_STAGE\" = remaining ] || exit 0; selector=terminal ;; *' -run ^TestUserJourney$/(claude|codex)$ '*) [ \"$NM_E2E_FAKE_GO_STAGE\" = matrix ] || exit 0; selector=claude-codex ;; *' -run ^TestUserJourney$ '*) [ \"$NM_E2E_FAKE_GO_STAGE\" = matrix ] || exit 0; selector=complement ;; *) exit 0 ;; esac\n"
	goScript += "export NM_E2E_FAKE_PARENT_PID=$$\n"
	goScript += "\"$NM_E2E_FAKE_CHILD\" \"$selector\" &\nchild_pid=$!\n"
	goScript += "trap 'wait \"$child_pid\" 2>/dev/null || true; printf \"parent-end:%s\\n\" \"$selector\" >>\"$NM_E2E_FAKE_GO_EVENTS\"; exit 0' TERM INT\n"
	goScript += "printf 'parent-ready:%s:%s:%s\\n' \"$selector\" \"$$\" \"$child_pid\" >\"$NM_E2E_FAKE_GO_READY\"\n"
	goScript += "wait \"$child_pid\"\n"
	if err := os.WriteFile(fakeGo, []byte(goScript), 0o755); err != nil {
		t.Fatal(err)
	}

	ready, err := os.OpenFile(readyFIFO, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer ready.Close()

	cmd := exec.Command("bash", "scripts/e2e.sh")
	cmd.Dir = filepath.Join("..", "..")
	cmd.Env = wrapperTestEnv(t, dir, binDir, "2", eventsPath)
	cmd.Env = append(cmd.Env,
		"NM_E2E_FAKE_GO_READY="+readyFIFO,
		"NM_E2E_FAKE_GO_GATE="+gateFIFO,
		"NM_E2E_FAKE_CHILD="+childPath,
		"NM_E2E_FAKE_GO_STAGE="+stage,
	)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start wrapper: %v", err)
	}
	waitDone := make(chan error, 1)
	go func() { waitDone <- cmd.Wait() }()
	var groupIDs []int
	wrapperFinished := false
	defer func() {
		if wrapperFinished {
			return
		}
		if len(groupIDs) > 0 {
			for _, groupID := range groupIDs {
				_ = syscall.Kill(-groupID, syscall.SIGTERM)
			}
		}
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-waitDone:
		case <-time.After(2 * time.Second):
			_ = cmd.Process.Kill()
		}
	}()

	readyCtx, cancelReady := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelReady()
	readyResult := make(chan []string, 1)
	go func() {
		reader := bufio.NewReader(ready)
		wantLines := 4
		if stage == "remaining" {
			// The terminal worker and the shard that runs beside it. The
			// fixture's daemon cap is 2, and the terminal worker takes one
			// slot, so exactly one shard runs beside it. The step-test
			// worker takes no daemon slot and is awaited BEFORE the
			// shards start, so it has already finished by then.
			wantLines = 4
		}
		lines := make([]string, 0, wantLines)
		for len(lines) < wantLines {
			line, readErr := reader.ReadString('\n')
			if readErr != nil {
				return
			}
			lines = append(lines, strings.TrimSpace(line))
		}
		readyResult <- lines
	}()
	var readyLines []string
	select {
	case readyLines = <-readyResult:
	case <-readyCtx.Done():
		t.Fatalf("timed out waiting for both fake jobs and children to report readiness: %v", readyCtx.Err())
	}
	wantJobs := 2
	if stage == "remaining" {
		wantJobs = 2
	}
	if len(readyLines) != wantJobs*2 {
		t.Fatalf("ready lines = %q, want %d parent and child readiness reports", readyLines, wantJobs)
	}
	childReadiness := 0
	for _, line := range readyLines {
		parts := strings.Split(line, ":")
		if len(parts) != 4 || (parts[0] != "parent-ready" && parts[0] != "child-ready") {
			t.Fatalf("malformed child readiness event %q", line)
		}
		if parts[0] == "child-ready" {
			childReadiness++
			continue
		}
		parentPID, err := strconv.Atoi(parts[2])
		if err != nil {
			t.Fatalf("parse fake go PID in %q: %v", line, err)
		}
		childPID, err := strconv.Atoi(parts[3])
		if err != nil {
			t.Fatalf("parse fake child PID in %q: %v", line, err)
		}
		groupIDs = append(groupIDs, parentPID)
		parentGroup := processGroupID(t, parentPID)
		childGroup := processGroupID(t, childPID)
		if parentGroup != strconv.Itoa(parentPID) || childGroup != parentGroup {
			t.Fatalf("wrapper job PIDs parent=%d group=%q child=%d group=%q; want each job's own process group", parentPID, parentGroup, childPID, childGroup)
		}
	}
	if childReadiness != wantJobs || len(groupIDs) != wantJobs {
		t.Fatalf("readiness events = %q, want %d child and parent reports", readyLines, wantJobs)
	}

	if err := cmd.Process.Signal(signal); err != nil {
		t.Fatalf("send targeted %s to wrapper: %v", signal, err)
	}
	termCtx, cancelTerm := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelTerm()
	var waitErr error
	select {
	case waitErr = <-waitDone:
		wrapperFinished = true
	case <-termCtx.Done():
		for _, groupID := range groupIDs {
			_ = syscall.Kill(-groupID, syscall.SIGTERM)
		}
		select {
		case <-waitDone:
			wrapperFinished = true
		case <-time.After(2 * time.Second):
			_ = cmd.Process.Kill()
		}
		t.Fatalf("wrapper did not exit promptly after TERM: %v", termCtx.Err())
	}
	if exitErr, ok := waitErr.(*exec.ExitError); !ok || exitErr.ExitCode() != expectedStatus {
		t.Fatalf("wrapper wait error = %v, want exit status %d", waitErr, expectedStatus)
	}
	eventsData, err := os.ReadFile(eventsPath)
	if err != nil {
		t.Fatalf("read signal events: %v", err)
	}
	events := strings.Split(strings.TrimSpace(string(eventsData)), "\n")
	cleanupIndex := -1
	childrenEnded := 0
	parentsEnded := 0
	for i, event := range events {
		if event == "cleanup" {
			cleanupIndex = i
		}
		if strings.HasPrefix(event, "child-end:") {
			childrenEnded++
		}
		if strings.HasPrefix(event, "parent-end:") {
			parentsEnded++
		}
	}
	if childrenEnded != wantJobs || parentsEnded != wantJobs || cleanupIndex != len(events)-1 {
		t.Fatalf("events = %q, want both descendants and both job leaders ended before cleanup", events)
	}
}

func processGroupID(t *testing.T, pid int) string {
	t.Helper()
	output, err := exec.Command("ps", "-o", "pgid=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		t.Fatalf("read process group for pid %d: %v", pid, err)
	}
	return strings.TrimSpace(string(output))
}

type e2eWrapperResult struct {
	status int
	output string
	calls  []string
	events []string
}

const wrapperDiscoveredTests = "TestUserJourney TestTargetAdvanceTerminalMergeLiveJourney TestRetainedTargetContinuityLiveJourney TestAttachCommitStatusJourney TestTargetAdvanceAcceptedGapComparisonOutageLiveJourney TestCIReplayOptionCannotBypassMovedComparisonOnRealBinary TestEvalJourney TestStepOnly TestCustomGateStructuredFindingsJourney TestCIScopeAutoDiscovery TestDocumentJourneyUnsafeAndStrictRestart TestOmitIntentJourney TestCertificationContentionProductionJourney"

// These are external go-list responses, not substitutes for the wrapper.
// Listing a steps-only name catches silently excluding an entire package.
//
// The inventory carries the MEASURED journeys as well as the small ones, so
// the shard placement is exercised against a representative distribution:
// a split that only balances a handful of equal-cost tests proves nothing
// about a bucket where one journey costs more than the rest together.
const wrapperDiscoveryScript = "case \" $* \" in *' -list '*) if [ \"${NM_E2E_FAKE_GO_FAIL_MATRIX:-}\" = discovery ]; then exit 23; fi; case \"$*\" in *'./internal/pipeline/steps/...'*) printf 'TestStepOnly\\n' ;; *) printf '%s\\n' 'TestUserJourney' 'TestTargetAdvanceTerminalMergeLiveJourney' 'TestRetainedTargetContinuityLiveJourney' 'TestAttachCommitStatusJourney' 'TestTargetAdvanceAcceptedGapComparisonOutageLiveJourney' 'TestCIReplayOptionCannotBypassMovedComparisonOnRealBinary' 'TestEvalJourney' 'TestCustomGateStructuredFindingsJourney' 'TestCIScopeAutoDiscovery' 'TestDocumentJourneyUnsafeAndStrictRestart' 'TestOmitIntentJourney' 'TestCertificationContentionProductionJourney' ;; esac; exit 0 ;; esac\n"

func runE2EWrapper(t *testing.T, failOnMatrix, explicit string) e2eWrapperResult {
	t.Helper()
	return runE2EWrapperWithCap(t, failOnMatrix, explicit, "2")
}

func runE2EWrapperWithCap(t *testing.T, failOnMatrix, explicit, cap string) e2eWrapperResult {
	t.Helper()
	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	if err := os.Mkdir(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(dir, "calls")
	eventsPath := filepath.Join(dir, "events")
	fakeGo := filepath.Join(binDir, "go")
	contents := "#!/bin/sh\nprintf '%s\\n' \"$*\" >>\"$NM_E2E_FAKE_GO_LOG\"\n"
	contents += "case \" $* \" in *' run ./internal/e2edaemon/reapmain.go '*) printf 'cleanup\\n' >>\"$NM_E2E_FAKE_GO_EVENTS\"; exit 0 ;; esac\n"
	contents += wrapperDiscoveryScript
	contents += "printf 'begin:%s\\n' \"$*\" >>\"$NM_E2E_FAKE_GO_EVENTS\"\nstatus=0\n"
	contents += "case \"$NM_E2E_FAKE_GO_FAIL_MATRIX:$*\" in first:*\"-run ^TestUserJourney$/(claude|codex)$\"*) status=17 ;; terminal:*\"-run ^(TestTargetAdvanceTerminalMergeLiveJourney|TestRetainedTargetContinuityLiveJourney)$\"*) status=19 ;; shard:*\"-skip ^(TestUserJourney|TestTargetAdvanceTerminalMergeLiveJourney|TestRetainedTargetContinuityLiveJourney)$\"*) status=29 ;; esac\n"
	contents += "printf 'end:%s:%s\\n' \"$*\" \"$status\" >>\"$NM_E2E_FAKE_GO_EVENTS\"\nexit \"$status\"\n"
	if err := os.WriteFile(fakeGo, []byte(contents), 0o755); err != nil {
		t.Fatal(err)
	}
	args := []string{}
	if explicit != "" {
		args = strings.Fields(explicit)
	}
	cmd := exec.Command("bash", append([]string{"scripts/e2e.sh"}, args...)...)
	cmd.Dir = filepath.Join("..", "..")
	cmd.Env = wrapperTestEnv(t, dir, binDir, cap, eventsPath)
	cmd.Env = append(cmd.Env, "NM_E2E_FAKE_GO_LOG="+logPath, "NM_E2E_FAKE_GO_FAIL_MATRIX="+failOnMatrix)
	output, err := cmd.CombinedOutput()
	status := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			status = exitErr.ExitCode()
		} else {
			t.Fatalf("run wrapper: %v", err)
		}
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read fake go log: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) == 1 && lines[0] == "" {
		lines = nil
	}
	eventData, err := os.ReadFile(eventsPath)
	if err != nil {
		t.Fatalf("read fake go events: %v", err)
	}
	events := strings.Split(strings.TrimSpace(string(eventData)), "\n")
	if len(events) == 1 && events[0] == "" {
		events = nil
	}
	return e2eWrapperResult{status: status, output: string(output), calls: lines, events: events}
}

func wrapperTestEnv(t *testing.T, dir, binDir, cap, eventsPath string) []string {
	t.Helper()
	env := make([]string, 0, len(os.Environ())+5)
	for _, entry := range os.Environ() {
		if strings.HasPrefix(entry, "PATH=") || strings.HasPrefix(entry, "NM_E2E_DAEMON_INVENTORY=") || strings.HasPrefix(entry, "NM_E2E_DAEMON_MAX=") || strings.HasPrefix(entry, "NM_E2E_FAKE_GO_") {
			continue
		}
		env = append(env, entry)
	}
	return append(env,
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"NM_E2E_DAEMON_INVENTORY="+filepath.Join(dir, "inventory"),
		"NM_E2E_DAEMON_MAX="+cap,
		"NM_E2E_FAKE_GO_EVENTS="+eventsPath,
	)
}
