package daemon

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/shellenv"
)

var daemonHealthCheck func(*paths.Paths, ...time.Duration) (bool, error) = daemonIsRunningViaIPC
var daemonDial = ipc.Dial
var daemonHealthDial = ipc.DialWithDeadline
var daemonStart = Start
var daemonProcessRunning = processRunning
var daemonProcessStartTime = processStartTime
var daemonKillPID = killPID
var daemonEndpointUsesRegularFile = func() bool { return runtime.GOOS == "windows" }

func daemonStartTimeout() time.Duration {
	// Unix readiness covers the missing-shell retry window, the final
	// login-shell probe, and the existing budget for recovery and other startup
	// work. Windows bypasses login-shell probing and keeps the original budget.
	fallback := 45 * time.Second
	if runtimeGOOS != "windows" {
		fallback += shellenv.DefaultShellRetryWindow + shellenv.DefaultShellProbeTimeout
	}
	return durationFromEnv("NM_TEST_DAEMON_START_TIMEOUT", fallback)
}

// daemonStopTimeout bounds how long waitForDaemonStop polls for a graceful
// shutdown before falling back to killing the daemon by PID.
// Windows gets a longer window: closing the loopback TCP listener used for
// its named-pipe-less IPC transport (transport_windows.go) does not make
// pending/racing connections fail as immediately as a Unix domain socket
// unlink does, so the health check can keep reporting an ambiguous error
// for longer after a graceful shutdown request has already succeeded.
func daemonStopTimeout() time.Duration {
	fallback := 5 * time.Second
	if runtimeGOOS == "windows" {
		fallback = 15 * time.Second
	}
	return durationFromEnv("NM_TEST_DAEMON_STOP_TIMEOUT", fallback)
}

func daemonStartPollInterval() time.Duration {
	return durationFromEnv("NM_TEST_DAEMON_START_POLL_INTERVAL", 100*time.Millisecond)
}

func daemonHealthReplyTimeout() time.Duration {
	return durationFromEnv("NM_TEST_DAEMON_HEALTH_REPLY_TIMEOUT", 5*time.Second)
}

func durationFromEnv(name string, fallback time.Duration) time.Duration {
	value := os.Getenv(name)
	if value == "" {
		return fallback
	}
	d, err := time.ParseDuration(value)
	if err != nil || d <= 0 {
		return fallback
	}
	return d
}

// Start installs or refreshes the managed daemon service when supported and
// starts it, falling back to a detached explicit `daemon run --root` re-exec
// when managed startup is unavailable or fails. Launch and readiness are
// distinct: readiness requires a real IPC health response within the bounded
// production startup budget.
//
// When the daemon is already running, Start refreshes the installed service
// definition and reloads the service manager if the on-disk definition is
// stale (e.g., after a binary upgrade that changed the plist/unit). This is
// what lets users pick up env-var changes (see #143 for the PATH fix) with
// a plain `daemon start` instead of a manual stop + start.
func Start(p *paths.Paths) error {
	return StartGuarded(p, nil)
}

// StartGuarded is Start, except that when a running daemon must be restarted
// to pick up a changed service definition, beforeRestart runs first; an error
// from it aborts the start before the definition is rewritten or the daemon
// is stopped.
func StartGuarded(p *paths.Paths, beforeRestart func() error) error {
	if err := p.EnsureDirs(); err != nil {
		return err
	}
	if alive, _ := daemonHealthCheck(p); alive {
		reloaded, err := reinstallManagedServiceIfChanged(p, beforeRestart)
		if err != nil {
			return err
		}
		if reloaded {
			return nil
		}
		return fmt.Errorf("daemon already running")
	}
	// Canonical socket is dead. A daemon for the same logical root may still be
	// alive under a different path spelling (symlinked or relative NM_HOME),
	// which the socket-keyed health check above cannot see. Detect via the OS
	// process list: refuse if a healthy stray is serving this root, or reap a
	// stale stray (including a crash-looping managed unit) before starting.
	if err := reconcileCollidingDaemons(p); err != nil {
		return err
	}
	var managedErr error
	if managed, err := installManagedService(p); err == nil {
		if managed {
			if err := startManagedDaemon(p); err == nil {
				return nil
			} else {
				managedErr = err
			}
			if cleanupErr := stopManagedFallback(p); cleanupErr != nil {
				return errors.Join(
					fmt.Errorf("managed startup failed: %w", managedErr),
					fmt.Errorf("managed cleanup failed: %w", cleanupErr),
				)
			}
		}
	} else {
		if alive, _ := daemonHealthCheck(p); alive {
			return nil
		}
		managedErr = fmt.Errorf("install managed service: %w", err)
	}

	fallbackErr := startDetachedDaemon(p)
	if fallbackErr == nil {
		return nil
	}
	if managedErr != nil {
		return errors.Join(
			fmt.Errorf("managed startup failed: %w", managedErr),
			fmt.Errorf("detached fallback failed: %w", fallbackErr),
		)
	}
	return fallbackErr
}

// reinstallManagedServiceIfChanged refreshes the managed daemon service and
// reloads it through launchctl/systemctl when the on-disk service definition
// differs from what the current binary would generate. Returns true when a
// reload actually happened. Called from Start() so that `daemon start` after
// a binary upgrade re-applies the new service definition without forcing
// users to run `daemon restart` explicitly. No-op on Windows and when the
// service manager is bypassed (i.e., under `go test`).
func reinstallManagedServiceIfChanged(p *paths.Paths, beforeRestart func() error) (bool, error) {
	if serviceManagerBypassed() {
		return false, nil
	}
	exe, err := serviceExecutablePath()
	if err != nil {
		return false, fmt.Errorf("resolve executable: %w", err)
	}
	home, err := serviceUserHomeDir()
	if err != nil {
		return false, fmt.Errorf("resolve user home: %w", err)
	}

	var installPath, wanted string
	renderedExecutable := exe
	switch runtimeGOOS {
	case "darwin":
		installPath = launchAgentPath(p)
	case "linux":
		installPath = systemdUserServicePath(p)
	default:
		return false, nil
	}

	existing, readErr := os.ReadFile(installPath)
	// Inherit any proxy already baked into the existing definition so an
	// env-less `daemon start` does not render a no-proxy target, falsely detect
	// drift, and reinstall - which would strip the proxy and re-break the daemon
	// with "403 Request not allowed". This mirrors the executable inheritance
	// below: prefer the current environment, fall back to what is on disk.
	var inheritedProxyEnv [][2]string
	if readErr == nil {
		switch runtimeGOOS {
		case "darwin":
			if existingExe, ok := launchAgentExecutable(existing); ok {
				renderedExecutable = existingExe
			}
			inheritedProxyEnv = launchAgentProxyEnv(existing)
		case "linux":
			if existingExe, ok := systemdUnitExecutable(existing); ok {
				renderedExecutable = existingExe
			}
			inheritedProxyEnv = systemdUnitProxyEnv(existing)
		}
	}
	proxyEnv := serviceProxyEnv()
	if len(proxyEnv) == 0 {
		proxyEnv = inheritedProxyEnv
	}
	switch runtimeGOOS {
	case "darwin":
		wanted = renderLaunchAgentWithProxyEnv(renderedExecutable, p, home, proxyEnv)
	case "linux":
		wanted = renderSystemdUnitWithProxyEnv(renderedExecutable, p, home, proxyEnv)
	}
	switch {
	case readErr == nil && string(existing) == wanted:
		return false, nil
	case os.IsNotExist(readErr):
		return false, nil
	case readErr != nil && !os.IsNotExist(readErr):
		return false, fmt.Errorf("read managed service definition: %w", readErr)
	}
	if beforeRestart != nil {
		if err := beforeRestart(); err != nil {
			return false, err
		}
	}
	restoreMode := os.FileMode(0o644)
	if info, err := os.Stat(installPath); err == nil {
		restoreMode = info.Mode().Perm()
	}
	stoppedForRefresh := false
	restoreOnFailure := func(cause error) (bool, error) {
		if err := writeFileAtomic(installPath, existing, restoreMode); err != nil {
			return false, fmt.Errorf("%w; restore managed service definition: %v", cause, err)
		}
		if err := reloadManagedServiceDefinition(p); err != nil {
			return false, fmt.Errorf("%w; reload restored managed service definition: %v", cause, err)
		}
		if stoppedForRefresh {
			if _, err := restartManagedService(p); err != nil {
				return false, fmt.Errorf("%w; restart restored managed service: %v", cause, err)
			}
		}
		return false, cause
	}

	if _, err := installManagedServiceWithExecutable(p, renderedExecutable); err != nil {
		return restoreOnFailure(err)
	}
	if err := stopCurrentDaemonBeforeManagedRestart(p); err != nil {
		return restoreOnFailure(err)
	}
	stoppedForRefresh = true
	if _, err := restartManagedService(p); err != nil {
		return restoreOnFailure(err)
	}
	if err := waitForDaemonStart(p, 0, time.Time{}); err != nil {
		return restoreOnFailure(err)
	}
	return true, nil
}

func stopCurrentDaemonBeforeManagedRestart(p *paths.Paths) error {
	return stopPlannedDaemon(p, nil)
}

func stopManagedFallback(p *paths.Paths) error {
	managed, err := stopManagedService(p)
	if !managed {
		return nil
	}
	if err == nil {
		if runtimeGOOS == "darwin" {
			if err := removeLaunchAgent(p); err != nil {
				return fmt.Errorf("remove launch agent before detached fallback: %w", err)
			}
		}
		if err := waitForManagedServiceExit(p, 5*time.Second); err != nil {
			return fmt.Errorf("wait for managed daemon exit before detached fallback: %w", err)
		}
		return nil
	}
	if alive, _ := daemonHealthCheck(p); alive {
		return fmt.Errorf("managed daemon is still running: %w", err)
	}
	return fmt.Errorf("stop managed daemon before detached fallback: %w", err)
}

// waitForManagedServiceExit closes the bootout/stop race before a detached
// fallback is launched. Health cannot prove exit for a child that never became
// ready, so inspect processes by canonical root and fail closed if the owned
// managed child survives the cleanup budget.
func waitForManagedServiceExit(p *paths.Paths, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	wantRoot := canonicalRoot(p.Root())
	for time.Now().Before(deadline) {
		processes, err := daemonListDaemonProcesses()
		if err != nil {
			return fmt.Errorf("enumerate daemon processes: %w", err)
		}
		var matching []int
		for _, process := range processes {
			if process.PID == os.Getpid() || canonicalRoot(process.Root) != wantRoot {
				continue
			}
			matching = append(matching, process.PID)
		}
		if len(matching) == 0 {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("managed daemon process for %s survived %v", p.Root(), timeout)
}

func startDetachedDaemon(p *paths.Paths) error {
	cleanupDaemonArtifacts(p)

	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve executable: %w", err)
	}

	logFile, err := os.OpenFile(p.DaemonBootstrapLog(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("open daemon bootstrap log: %w", err)
	}
	defer logFile.Close()

	cmd := exec.Command(exe, "daemon", "run", "--root", p.Root())
	cmd.Env = upsertEnv(os.Environ(), "NM_HOME", p.Root())
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	// Detach from parent process group so daemon survives CLI exit.
	setSysProcAttr(cmd)

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start daemon: %w", err)
	}

	pid := cmd.Process.Pid
	startedAt, err := daemonProcessStartTime(pid)
	if err != nil {
		if cleanupErr := cleanupStartedDaemonProcess(cmd.Process); cleanupErr != nil {
			return fmt.Errorf("inspect daemon process %d: %w; cleanup daemon child: %v", pid, err, cleanupErr)
		}
		return fmt.Errorf("inspect daemon process %d: %w", pid, err)
	}
	slog.Info("daemon process launched", "pid", pid, "log", p.DaemonLog(), "bootstrap_log", p.DaemonBootstrapLog())

	// Own Wait in a goroutine from launch onward. This reports a genuine child
	// exit immediately and guarantees timeout cleanup reaps the exact child,
	// rather than leaving a zombie or a process racing fallback rollback.
	exitCh := make(chan error, 1)
	go func() {
		state, waitErr := cmd.Process.Wait()
		if waitErr == nil && state != nil && !state.Success() {
			waitErr = fmt.Errorf("exit status %d", state.ExitCode())
		}
		exitCh <- waitErr
	}()
	return waitForDaemonStartWithProcess(p, cmd.Process, exitCh, pid, startedAt, managedServiceLaunch{})
}

func startManagedDaemon(p *paths.Paths) error {
	launch, err := prepareManagedDaemonLaunch(p)
	if err != nil {
		return fmt.Errorf("inspect managed daemon before launch: %w", err)
	}
	if _, err := startManagedService(p); err != nil {
		if alive, _ := daemonHealthCheck(p); alive {
			return nil
		}
		return err
	}
	slog.Info("managed daemon launched")
	return waitForDaemonStartWithProcess(p, nil, nil, 0, time.Time{}, launch)
}

func waitForDaemonStart(p *paths.Paths, pid int, startedAt time.Time) error {
	return waitForDaemonStartWithProcess(p, nil, nil, pid, startedAt, managedServiceLaunch{})
}

func waitForDaemonStartWithProcess(p *paths.Paths, proc *os.Process, exitCh <-chan error, pid int, startedAt time.Time, launch managedServiceLaunch) error {
	timeout := daemonStartTimeout()
	pollInterval := daemonStartPollInterval()
	deadline := time.Now().Add(timeout)
	managedPID := 0
	nextManagedProbe := time.Time{}
	var lastHealthErr error

	for time.Now().Before(deadline) {
		remaining := time.Until(deadline)
		if alive, err := daemonHealthCheck(p, remaining); alive {
			slog.Info("daemon ready", "pid", pid)
			return nil
		} else if err != nil {
			lastHealthErr = err
		}

		if exitCh != nil {
			select {
			case exitErr := <-exitCh:
				if exitErr != nil {
					return fmt.Errorf("daemon child %d exited before readiness: %w", pid, exitErr)
				}
				return fmt.Errorf("daemon child %d exited before readiness", pid)
			default:
			}
		} else if pid == 0 {
			if state, err := inspectManagedDaemonService(p, launch); err == nil && state == managedServiceExited {
				return fmt.Errorf("managed daemon exited before readiness")
			}
			// Managed services have no os.Process handle. The daemon publishes its
			// PID immediately after taking the singleton lock, before recovery, so
			// observe that exact child and notice an early exit without waiting out
			// the full readiness budget.
			if managedPID == 0 {
				if record, err := readDaemonPIDFile(p.PIDFile()); err == nil && !record.StartedAt.IsZero() {
					if actual, err := daemonProcessStartTime(record.PID); err == nil {
						if matches, _ := daemonPIDRecordMatchesProcess(p, record, actual); matches {
							managedPID = record.PID
							nextManagedProbe = time.Now().Add(250 * time.Millisecond)
						}
					}
				}
			} else if !time.Now().Before(nextManagedProbe) {
				running, err := daemonProcessRunning(managedPID)
				if err == nil && !running {
					return fmt.Errorf("managed daemon child %d exited before readiness", managedPID)
				}
				nextManagedProbe = time.Now().Add(250 * time.Millisecond)
			}
		}
		if remaining := time.Until(deadline); pollInterval > remaining {
			pollInterval = remaining
		}
		if pollInterval > 0 {
			time.Sleep(pollInterval)
		}
	}

	timeoutErr := fmt.Errorf("daemon launched but did not become ready within %v", timeout)
	if lastHealthErr != nil {
		timeoutErr = fmt.Errorf("%w: last health check: %v", timeoutErr, lastHealthErr)
	}

	cleanupWait := 5 * time.Second
	if timeout < cleanupWait {
		cleanupWait = timeout
	}

	// Kill and reap the detached child so it cannot race rollback or a fallback
	// service after the caller gives up. Managed cleanup is owned by Start.
	if proc != nil && pid > 0 {
		if err := proc.Kill(); err != nil {
			select {
			case <-exitCh:
				return timeoutErr
			default:
				return fmt.Errorf("%w: cleanup daemon child %d: %v", timeoutErr, pid, err)
			}
		}
		select {
		case <-exitCh:
			return timeoutErr
		case <-time.After(cleanupWait):
			return fmt.Errorf("%w: cleanup daemon child %d: wait for exit timed out", timeoutErr, pid)
		}
	}

	// Retain the explicit-PID cleanup path used by callers that own a known
	// detached process but do not have an os.Process handle.
	if pid > 0 {
		if err := killTimedOutDaemonPID(pid, startedAt); err != nil {
			return fmt.Errorf("%w: cleanup daemon child %d: %v", timeoutErr, pid, err)
		}
		if !startedAt.IsZero() {
			waitForProcessExit(pid, cleanupWait)
		}
	}
	return timeoutErr
}

func cleanupStartedDaemonProcess(proc *os.Process) error {
	if proc == nil {
		return nil
	}
	if err := proc.Kill(); err != nil {
		if _, waitErr := proc.Wait(); waitErr == nil {
			return nil
		}
		return fmt.Errorf("kill daemon child: %w", err)
	}
	if _, err := proc.Wait(); err != nil {
		return fmt.Errorf("wait daemon child exit: %w", err)
	}
	return nil
}

func killTimedOutDaemonPID(pid int, startedAt time.Time) error {
	if pid <= 0 || startedAt.IsZero() {
		return nil
	}
	currentStartTime, err := daemonProcessStartTime(pid)
	if err != nil {
		return fmt.Errorf("inspect daemon pid %d before kill: %w", pid, err)
	}
	if !currentStartTime.Equal(startedAt) {
		return fmt.Errorf("daemon pid %d no longer matches original process", pid)
	}
	return daemonKillPID(pid)
}

func waitForProcessExit(pid int, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		running, err := daemonProcessRunning(pid)
		if err == nil && !running {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// IsRunning checks if the daemon is alive by sending a health check via IPC.
func IsRunning(p *paths.Paths) (bool, error) {
	return daemonHealthCheck(p)
}

func daemonIsRunningViaIPC(p *paths.Paths, budget ...time.Duration) (bool, error) {
	deadline := time.Time{}
	if len(budget) > 0 {
		if budget[0] <= 0 {
			return false, fmt.Errorf("daemon health check deadline exceeded")
		}
		deadline = time.Now().Add(budget[0])
	}
	client, err := daemonHealthDial(p.Socket(), deadline)
	if err != nil {
		_, statErr := os.Stat(p.Socket())
		if os.IsNotExist(statErr) {
			return false, nil
		}
		if ipc.IsConnectTimeout(err) {
			return false, err
		}
		if statErr != nil {
			return false, fmt.Errorf("check daemon socket: %w", statErr)
		}
		if cleared, clearErr := clearDeadDaemonArtifacts(p); clearErr == nil && cleared {
			return false, nil
		}
		return false, fmt.Errorf("connect to daemon socket: %w", err)
	}
	defer client.Close()

	var result ipc.HealthResult
	timeout := daemonHealthReplyTimeout()
	if !deadline.IsZero() {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return false, fmt.Errorf("daemon health check deadline exceeded")
		}
		if timeout > remaining {
			timeout = remaining
		}
	}
	if err := client.CallWithTimeout(ipc.MethodHealth, &ipc.HealthParams{}, &result, timeout); err != nil {
		return false, err
	}
	return result.Status == "ok", nil
}

// Stop sends a shutdown request to the running daemon and waits for it to exit.
func Stop(p *paths.Paths) error { return stopPlannedDaemon(p, nil) }

func stopDetachedDaemon(p *paths.Paths) error { return stopPlannedDaemon(p, nil) }

// The same instance-bound preparation covers planned stop and managed refresh.
// The supervisor is disabled only after IPC has drained the run decisions.
func stopPlannedDaemon(p *paths.Paths, cleanup func() error) error {
	instance := captureRunningDaemon(p)
	if instance.pid > 0 {
		if exited, err := instance.exited(); err == nil && exited {
			return stopCapturedSupervisor(p, instance, cleanup)
		}
	}
	if instance.pid <= 0 || instance.startedAt.IsZero() {
		record, err := readDaemonPIDFile(p.PIDFile())
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return stopCapturedSupervisor(p, daemonInstance{}, cleanup)
			}
			return err
		}
		running, inspectErr := daemonProcessRunning(record.PID)
		if inspectErr != nil || running {
			return errors.Join(fmt.Errorf("captured daemon identity is unavailable"), inspectErr)
		}
		stale := daemonInstance{pid: record.PID, startedAt: record.StartedAt, instance: record.Instance}
		return stopCapturedSupervisor(p, stale, cleanup)
	}
	client, dialErr := daemonDial(p.Socket())
	var requestErr error
	if dialErr == nil {
		// Recheck after dial so replacing the root's socket/PID record cannot
		// redirect an unbound legacy request to a successor.
		if err := verifyCapturedDaemonRecord(p, instance); err != nil {
			_ = client.Close()
			return err
		}
		expected := ipc.ShutdownParams{ExpectedPID: instance.pid, ExpectedStartedAt: instance.startedAt.UTC().Format(time.RFC3339Nano), ExpectedInstance: instance.instance}
		legacy := instance.instance == ""
		if legacy {
			if err := verifyLegacyDaemon(p, instance); err != nil {
				_ = client.Close()
				return err
			}
			// Older daemons have no preservation protocol. An empty request
			// asks only for their ordinary, non-preserving shutdown.
			expected = ipc.ShutdownParams{}
		}
		var result ipc.ShutdownResult
		requestErr = client.CallWithTimeout(ipc.MethodShutdown, &expected, &result, 35*time.Second)
		_ = client.Close()
		if requestErr == nil {
			if legacy {
				if !result.OK || result.PID != 0 || result.StartedAt != "" || result.Instance != "" || result.Lifecycle != "" {
					requestErr = fmt.Errorf("unexpected legacy daemon shutdown acknowledgement")
				}
			} else if !result.OK || result.PID != expected.ExpectedPID || result.StartedAt != expected.ExpectedStartedAt || result.Instance != expected.ExpectedInstance || result.Lifecycle == "" {
				requestErr = fmt.Errorf("daemon shutdown acknowledgement identity is incomplete or changed")
			}
		}
	} else {
		requestErr = fmt.Errorf("dial daemon shutdown: %w", dialErr)
	}
	if requestErr != nil {
		return requestErr
	}
	if err := verifyCapturedDaemonRecord(p, instance); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := waitForDaemonStop(p, instance); err != nil {
		return err
	}
	return stopCapturedSupervisor(p, instance, cleanup)
}

func stopCapturedSupervisor(p *paths.Paths, instance daemonInstance, cleanup func() error) error {
	lock, err := acquireSingletonLock(p)
	if err != nil {
		return err
	}
	defer lock.Release()
	verify := func() error {
		err := verifyCapturedDaemonRecord(p, instance)
		if err == nil || errors.Is(err, os.ErrNotExist) {
			if instance.pid <= 0 {
				if _, socketErr := os.Stat(p.Socket()); !os.IsNotExist(socketErr) {
					return fmt.Errorf("uncaptured daemon socket remains: %v", socketErr)
				}
			}
			return nil
		}
		return err
	}
	if err := verify(); err != nil {
		return err
	}
	if _, err := stopManagedService(p); err != nil {
		return err
	}
	if err := verify(); err != nil {
		return err
	}
	cleanupDaemonArtifacts(p)
	if cleanup != nil {
		return cleanup()
	}
	return nil
}

// verifyCapturedDaemonRecord refuses to act on a successor's service or socket.
func verifyCapturedDaemonRecord(p *paths.Paths, instance daemonInstance) error {
	record, err := readDaemonPIDFile(p.PIDFile())
	if err != nil {
		return err
	}
	if record.PID != instance.pid || !record.StartedAt.Equal(instance.startedAt) || record.Instance != instance.instance {
		return fmt.Errorf("captured daemon identity changed")
	}
	return nil
}

// A pre-instance runtime cannot acknowledge certified preservation. Its legacy
// RPC is allowed only with independent OS proof of the PID, start and root;
// current runtimes still require the complete identity-bound handshake.
func verifyLegacyDaemon(p *paths.Paths, instance daemonInstance) error {
	startedAt, err := daemonProcessStartTime(instance.pid)
	if err != nil {
		return fmt.Errorf("inspect legacy daemon: %w", err)
	}
	if diff := startedAt.Sub(instance.startedAt); diff < -orphanStartTimeTolerance || diff > orphanStartTimeTolerance {
		return fmt.Errorf("legacy daemon start time changed")
	}
	processes, err := daemonListDaemonProcesses()
	if err != nil {
		return fmt.Errorf("inspect legacy daemon root: %w", err)
	}
	for _, process := range processes {
		if process.PID == instance.pid && canonicalRoot(process.Root) == canonicalRoot(p.Root()) {
			return nil
		}
	}
	return fmt.Errorf("legacy daemon PID does not own the captured root")
}

// daemonInstance names the exact daemon process a stop is responsible for.
// Current runtimes require the complete planned-shutdown tuple. Pre-instance
// runtimes require independent PID/start/root verification for a legacy stop;
// IPC health alone cannot authorize either path.
type daemonInstance struct {
	instance  string
	pid       int
	startedAt time.Time
}

// captureRunningDaemon best-effort identifies the live daemon process for this
// root from the PID file, validated against the process's real start time so a
// recycled PID cannot be mistaken for the daemon. It must be called before a
// shutdown request, while the complete runtime record still exists. In-process
// test daemons are captured for IPC; the fallback never kills our own process.
func captureRunningDaemon(p *paths.Paths) daemonInstance {
	record, err := readDaemonPIDFile(p.PIDFile())
	if err != nil || record.PID <= 0 {
		return daemonInstance{}
	}
	if record.StartedAt.IsZero() {
		return daemonInstance{pid: record.PID, instance: record.Instance}
	}
	startedAt, err := daemonProcessStartTime(record.PID)
	if err != nil {
		return daemonInstance{pid: record.PID, startedAt: record.StartedAt.UTC(), instance: record.Instance}
	}
	matches, err := daemonPIDRecordMatchesProcess(p, record, startedAt)
	if err != nil {
		return daemonInstance{pid: record.PID, instance: record.Instance}
	}
	if !matches {
		return daemonInstance{}
	}
	return daemonInstance{pid: record.PID, startedAt: record.StartedAt.UTC(), instance: record.Instance}
}

// exited reports whether this instance is gone. A PID now owned by a different
// process is gone. A zero instance has no process identity to inspect, so the
// caller relies on the independent health check.
func (i daemonInstance) exited() (bool, error) {
	if i.pid <= 0 {
		return true, nil
	}
	running, err := daemonProcessRunning(i.pid)
	if err != nil {
		return false, err
	}
	if !running {
		return true, nil
	}
	if i.startedAt.IsZero() {
		return false, fmt.Errorf("daemon pid %d start time is unknown", i.pid)
	}
	startedAt, err := daemonProcessStartTime(i.pid)
	if err != nil {
		// `ps` also fails for a PID that has just exited: a daemon draining its
		// runs can outlast the stop timeout and exit while it is inspected.
		if running, runErr := daemonProcessRunning(i.pid); runErr == nil && !running {
			return true, nil
		}
		return false, err
	}
	diff := startedAt.Sub(i.startedAt)
	if diff < 0 {
		diff = -diff
	}
	return diff > orphanStartTimeTolerance, nil
}

// errDaemonPIDExited means the PID fallback found the daemon process already
// gone while it was being inspected, so the stop has nothing left to kill.
var errDaemonPIDExited = errors.New("daemon process exited")

func validateDaemonPIDFallback(p *paths.Paths, pid int) error {
	if pid <= 0 {
		return fmt.Errorf("invalid daemon pid %d", pid)
	}
	// A daemon is always a separate process from whatever is stopping it.
	// A pid file recording our own pid means the "daemon" is actually
	// running in-process (e.g. a test harness driving RunWithResources in
	// a goroutine) rather than as a real detached process. Killing it here
	// would kill the caller, not a daemon.
	if pid == os.Getpid() {
		return fmt.Errorf("refusing to kill our own pid %d", pid)
	}
	record, err := readDaemonPIDFile(p.PIDFile())
	if err != nil {
		return fmt.Errorf("read pid file: %w", err)
	}
	if record.PID != pid {
		return fmt.Errorf("daemon pid %d does not match pid file instance", pid)
	}
	startTime, err := daemonProcessStartTime(pid)
	if err != nil {
		// `ps` also fails for a PID that has just exited: a daemon draining its
		// runs can outlast the stop timeout and exit while it is inspected. It
		// is gone only when the process is absent and the socket no longer
		// answers either.
		if running, runErr := daemonProcessRunning(pid); runErr == nil && !running {
			if alive, healthErr := daemonHealthCheck(p); healthErr == nil && !alive {
				return fmt.Errorf("%w: inspect daemon pid %d: %v", errDaemonPIDExited, pid, err)
			}
		}
		return fmt.Errorf("inspect daemon pid %d: %w", pid, err)
	}
	matches, err := daemonPIDRecordMatchesProcess(p, record, startTime)
	if err != nil {
		return fmt.Errorf("validate daemon pid %d: %w", pid, err)
	}
	if !matches {
		return fmt.Errorf("daemon pid %d does not match pid file instance", pid)
	}
	return nil
}

func killPID(pid int) error {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return fmt.Errorf("find process: %w", err)
	}
	return proc.Kill()
}

func staleDaemonArtifacts(p *paths.Paths) (bool, error) {
	info, err := os.Stat(p.Socket())
	missingSocket := os.IsNotExist(err)
	if err != nil && !missingSocket {
		return false, fmt.Errorf("stat daemon socket: %w", err)
	}
	if err == nil && info.Mode()&os.ModeSocket == 0 && !daemonEndpointUsesRegularFile() {
		return true, nil
	}
	pid, err := ReadPID(p)
	if err != nil {
		if os.IsNotExist(err) {
			if missingSocket {
				return true, nil
			}
			if daemonEndpointUsesRegularFile() {
				return false, nil
			}
			alive, err := daemonSocketAcceptingConnections(p.Socket())
			if err != nil {
				return false, err
			}
			return !alive, nil
		}
		return false, err
	}
	running, err := daemonProcessRunning(pid)
	if err != nil {
		return false, err
	}
	if missingSocket && running {
		return false, nil
	}
	return !running, nil
}

func daemonSocketAcceptingConnections(path string) (bool, error) {
	conn, err := net.DialTimeout("unix", path, 200*time.Millisecond)
	if err != nil {
		return false, nil
	}
	defer conn.Close()
	return true, nil
}

// waitForDaemonStop waits until the daemon instance this stop is responsible
// for is really gone, then clears its artifacts.
//
// For a captured process, exit is authoritative even if successor IPC is healthy.
// Losing IPC health alone is NOT sufficient proof of exit. The daemon
// closes its IPC listener at the START of shutdown, and closing a Unix
// listener unlinks the socket, so the health check reports "not running"
// while the process is still draining runs, closing the database, flushing
// logs, and reaping its bootstrap log-sink child. The NM_HOME singleton lock
// is an OS file lock the kernel releases only when the owning process
// actually dies, so a stop that returned at "socket gone" handed the caller a
// root whose lock was still held - and the very next `daemon start` (that is,
// `daemon restart`) launched a child that failed acquireSingletonLock and
// exited before readiness with status 1. Waiting for the captured instance to
// exit is what makes "daemon stopped" mean the daemon's resources are free.
func waitForDaemonStop(p *paths.Paths, instance daemonInstance) error {
	deadline := time.Now().Add(daemonStopTimeout())
	for time.Now().Before(deadline) {
		if instance.pid == os.Getpid() {
			record, err := readDaemonPIDFile(p.PIDFile())
			if err == nil && record.Instance != "" && record.Instance != instance.instance {
				return nil
			}
			if os.IsNotExist(err) {
				if lock, lockErr := acquireSingletonLock(p); lockErr == nil {
					lock.Release()
					return nil
				}
			}
		} else if instance.pid > 0 {
			exited, exitErr := instance.exited()
			if exitErr == nil && exited {
				cleanupCapturedDaemonArtifacts(p, instance)
				slog.Info("daemon stopped gracefully")
				return nil
			}
		} else if alive, err := daemonHealthCheck(p); err == nil && !alive {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}

	if instance.pid == os.Getpid() {
		return fmt.Errorf("refusing to kill our own pid %d", instance.pid)
	}
	if instance.pid <= 0 {
		if record, err := readDaemonPIDFile(p.PIDFile()); err == nil && record.PID == os.Getpid() {
			return fmt.Errorf("refusing to kill our own pid %d", record.PID)
		}
	}
	if instance.pid <= 0 {
		if alive, healthErr := daemonHealthCheck(p); healthErr == nil && !alive {
			cleanupCapturedDaemonArtifacts(p, instance)
			return nil
		}
	}
	if instance.pid <= 0 || instance.startedAt.IsZero() {
		return fmt.Errorf("daemon did not stop within timeout: captured process identity is unavailable")
	}
	exited, exitErr := instance.exited()
	if exitErr != nil {
		return fmt.Errorf("inspect captured daemon: %w", exitErr)
	}
	if exited {
		// A known successor does not belong to this stop. Without its record,
		// retain the upstream deadline-boundary check for an absent endpoint.
		record, readErr := readDaemonPIDFile(p.PIDFile())
		successor := readErr == nil && (record.PID != instance.pid || !record.StartedAt.Equal(instance.startedAt) || record.Instance != instance.instance)
		if !successor {
			if alive, healthErr := daemonHealthCheck(p); healthErr != nil || alive {
				return fmt.Errorf("daemon did not stop within timeout: endpoint exit could not be verified")
			}
		}
		cleanupCapturedDaemonArtifacts(p, instance)
		return nil
	}
	// Validate the captured process itself; a successor PID file is never a target.
	matches, err := daemonPIDRecordMatchesProcess(p, daemonPIDFile{PID: instance.pid, StartedAt: instance.startedAt}, instance.startedAt)
	if err != nil || !matches {
		return fmt.Errorf("captured daemon identity could not be verified: %v", err)
	}
	pid := instance.pid

	slog.Warn("daemon did not stop gracefully, killing", "pid", pid)
	if err := daemonKillPID(pid); err != nil {
		return fmt.Errorf("kill daemon pid %d: %w", pid, err)
	}

	killDeadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(killDeadline) {
		exited, err := instance.exited()
		if err != nil {
			return err
		}
		if exited {
			cleanupCapturedDaemonArtifacts(p, instance)
			slog.Warn("daemon killed after shutdown timeout", "pid", pid)
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("daemon pid %d still running after kill", pid)
}

func cleanupCapturedDaemonArtifacts(p *paths.Paths, instance daemonInstance) {
	lock, err := acquireSingletonLock(p)
	if err != nil {
		return
	}
	defer lock.Release()
	record, err := readDaemonPIDFile(p.PIDFile())
	if os.IsNotExist(err) {
		// The captured daemon may remove its PID file before the final poll.
		// Holding the singleton and proving the endpoint down protects a successor.
		if alive, healthErr := daemonHealthCheck(p); healthErr == nil && !alive {
			cleanupDaemonArtifacts(p)
		}
		return
	}
	if err != nil || record.PID != instance.pid || !record.StartedAt.Equal(instance.startedAt) || record.Instance != instance.instance {
		return
	}
	cleanupDaemonArtifacts(p)
}

// clearDeadDaemonArtifacts removes the socket and PID file of a daemon that
// is provably gone: the PID file names a process that no longer exists or
// whose start time no longer matches, and no process holds the NM_HOME
// singleton lock. A live or unverifiable PID leaves everything in place. The
// lock is held only to re-check the PID file and remove the artifacts, so a
// daemon that is starting up is neither blocked by the inspection nor loses
// the socket it just bound.
func clearDeadDaemonArtifacts(p *paths.Paths) (bool, error) {
	data, err := os.ReadFile(p.PIDFile())
	if err != nil {
		return false, fmt.Errorf("read pid file: %w", err)
	}
	record, err := readDaemonPIDFileData(data)
	if err != nil {
		return false, fmt.Errorf("read pid file: %w", err)
	}
	running, err := daemonProcessRunning(record.PID)
	if err != nil {
		return false, fmt.Errorf("inspect daemon pid %d: %w", record.PID, err)
	}
	if running {
		if record.StartedAt.IsZero() {
			return false, nil
		}
		startedAt, err := daemonProcessStartTime(record.PID)
		if err != nil {
			return false, fmt.Errorf("inspect daemon pid %d: %w", record.PID, err)
		}
		diff := startedAt.Sub(record.StartedAt.UTC())
		if diff < 0 {
			diff = -diff
		}
		if diff <= orphanStartTimeTolerance {
			return false, nil
		}
	}

	f, err := os.OpenFile(p.LockFile(), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return false, fmt.Errorf("open daemon lock: %w", err)
	}
	defer f.Close()
	if err := tryLockFile(f); err != nil {
		return false, nil
	}
	defer func() { _ = unlockFile(f) }()
	current, err := os.ReadFile(p.PIDFile())
	if err != nil || !bytes.Equal(current, data) {
		return false, nil
	}
	cleanupDaemonArtifacts(p)
	return true, nil
}

func cleanupDaemonArtifacts(p *paths.Paths) {
	_ = os.Remove(p.Socket())
	_ = os.Remove(p.PIDFile())
}

func upsertEnv(env []string, key, value string) []string {
	prefix := key + "="
	updated := false
	result := make([]string, 0, len(env)+1)
	for _, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			if !updated {
				result = append(result, prefix+value)
				updated = true
			}
			continue
		}
		result = append(result, entry)
	}
	if !updated {
		result = append(result, prefix+value)
	}
	return result
}

// EnsureDaemon starts the daemon if it's not already running.
func EnsureDaemon(p *paths.Paths) error {
	if err := ipc.CheckEndpointPath(p.Socket()); err != nil {
		return err
	}
	alive, err := daemonHealthCheck(p)
	if err != nil {
		return fmt.Errorf("%w (run 'no-mistakes daemon start' to recover)", err)
	}
	if alive {
		return nil
	}
	return daemonStart(p)
}

// ReadPID reads the daemon PID from the PID file.
func ReadPID(p *paths.Paths) (int, error) {
	record, err := readDaemonPIDFile(p.PIDFile())
	if err != nil {
		return 0, err
	}
	return record.PID, nil
}
