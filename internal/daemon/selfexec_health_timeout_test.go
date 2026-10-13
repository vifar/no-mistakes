package daemon

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
)

func listenHealthTestSocket(t *testing.T, p *paths.Paths, delay time.Duration, answer bool) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("Unix socket test")
	}
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				_, _ = bufio.NewReader(conn).ReadBytes('\n')
				if delay > 0 {
					time.Sleep(delay)
				}
				if answer {
					_, _ = fmt.Fprintln(conn, `{"jsonrpc":"2.0","result":{"status":"ok"},"id":1}`)
					return
				}
				// Hold the accepted connection until the client enforces its
				// reply deadline and closes it.
				_, _ = io.Copy(io.Discard, conn)
			}(conn)
		}
	}()
}

func TestIsRunningAcceptsSlowHealthReply(t *testing.T) {
	root, err := os.MkdirTemp("/tmp", "nm-health-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	p := paths.WithRoot(root)
	listenHealthTestSocket(t, p, 350*time.Millisecond, true)

	started := time.Now()
	alive, err := IsRunning(p)
	if err != nil || !alive {
		t.Fatalf("IsRunning() = %v, %v; want healthy slow reply", alive, err)
	}
	if elapsed := time.Since(started); elapsed < 300*time.Millisecond {
		t.Fatalf("health reply returned in %v; test server delay was not observed", elapsed)
	}
}

func TestEnsureDaemonAcceptsSlowHealthReplyWithoutSpawning(t *testing.T) {
	root, err := os.MkdirTemp("/tmp", "nm-health-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	p := paths.WithRoot(root)
	listenHealthTestSocket(t, p, 500*time.Millisecond, true)
	oldStart := daemonStart
	started := false
	daemonStart = func(*paths.Paths) error { started = true; return nil }
	t.Cleanup(func() { daemonStart = oldStart })
	startedAt := time.Now()
	if err := EnsureDaemon(p); err != nil {
		t.Fatalf("EnsureDaemon = %v, want successful slow health reply", err)
	}
	if elapsed := time.Since(startedAt); elapsed < 400*time.Millisecond {
		t.Fatalf("EnsureDaemon elapsed %v, want observed slow reply", elapsed)
	}
	if started {
		t.Fatal("EnsureDaemon spawned despite successful health reply")
	}
}

func TestEnsureDaemonDoesNotSpawnOnUnansweredHealthReply(t *testing.T) {
	root, err := os.MkdirTemp("/tmp", "nm-health-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	p := paths.WithRoot(root)
	t.Setenv("NM_TEST_DAEMON_HEALTH_REPLY_TIMEOUT", "120ms")
	listenHealthTestSocket(t, p, 0, false)

	oldStart := daemonStart
	started := false
	daemonStart = func(*paths.Paths) error { started = true; return nil }
	t.Cleanup(func() { daemonStart = oldStart })

	startedAt := time.Now()
	err = EnsureDaemon(p)
	elapsed := time.Since(startedAt)
	if err == nil {
		t.Fatal("EnsureDaemon succeeded for unanswered health request")
	}
	if !ipc.IsCallTimeout(err) {
		t.Fatalf("EnsureDaemon error = %v, want IPC call timeout", err)
	}
	if elapsed < 100*time.Millisecond {
		t.Fatalf("EnsureDaemon elapsed %v, want at least the 120ms reply timeout", elapsed)
	}
	if started {
		t.Fatal("EnsureDaemon spawned a daemon after an ambiguous health reply")
	}
}

func TestWaitForDaemonStartBoundsHealthProbeByStartupDeadline(t *testing.T) {
	root, err := os.MkdirTemp("/tmp", "nm-health-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	p := paths.WithRoot(root)
	t.Setenv("NM_TEST_DAEMON_START_TIMEOUT", "160ms")
	t.Setenv("NM_TEST_DAEMON_START_POLL_INTERVAL", "10ms")
	t.Setenv("NM_TEST_DAEMON_HEALTH_REPLY_TIMEOUT", "5s")
	listenHealthTestSocket(t, p, 0, false)

	started := time.Now()
	err = waitForDaemonStart(p, 0, time.Time{})
	elapsed := time.Since(started)
	if err == nil {
		t.Fatal("waitForDaemonStart succeeded without health response")
	}
	if !strings.Contains(err.Error(), "daemon health did not reply") {
		t.Fatalf("readiness error = %v, want IPC call timeout evidence", err)
	}
	if elapsed < 140*time.Millisecond || elapsed > 2500*time.Millisecond {
		t.Fatalf("waitForDaemonStart took %v, want startup deadline to cap health probes", elapsed)
	}
}

func TestWaitForDaemonStartBoundsConnectionByStartupDeadline(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.Socket(), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NM_TEST_DAEMON_START_TIMEOUT", "160ms")
	t.Setenv("NM_TEST_DAEMON_START_POLL_INTERVAL", "10ms")
	oldDial := daemonHealthDial
	calls := 0
	daemonHealthDial = func(socket string, deadline time.Time) (*ipc.Client, error) {
		calls++
		if socket != p.Socket() {
			t.Fatalf("dial socket = %q, want %q", socket, p.Socket())
		}
		if deadline.IsZero() || time.Until(deadline) > 160*time.Millisecond {
			t.Fatalf("readiness dial deadline = %v, want remaining startup budget", deadline)
		}
		remaining := time.Until(deadline)
		if remaining > 0 {
			time.Sleep(remaining)
		}
		return nil, &ipc.ConnectTimeoutError{SocketPath: socket, TimeoutDuration: remaining, Err: os.ErrDeadlineExceeded}
	}
	t.Cleanup(func() { daemonHealthDial = oldDial })
	started := time.Now()
	err := waitForDaemonStart(p, 0, time.Time{})
	if err == nil || !strings.Contains(err.Error(), "did not accept a connection") {
		t.Fatalf("readiness error = %v, want connection timeout evidence", err)
	}
	if calls == 0 {
		t.Fatal("readiness never attempted a connection")
	}
	if elapsed := time.Since(started); elapsed < 140*time.Millisecond || elapsed > 2500*time.Millisecond {
		t.Fatalf("readiness took %v, want connection capped by startup budget", elapsed)
	}
	if _, err := os.Stat(p.Socket()); err != nil {
		t.Fatalf("connection timeout removed socket: %v", err)
	}
}
