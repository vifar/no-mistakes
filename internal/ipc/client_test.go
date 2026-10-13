package ipc

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

type timeoutDialError struct{}

func (timeoutDialError) Error() string   { return "dial timeout" }
func (timeoutDialError) Timeout() bool   { return true }
func (timeoutDialError) Temporary() bool { return true }

func TestDialConnectTimeoutFailsFastAndNamesSocket(t *testing.T) {
	const timeout = 25 * time.Millisecond
	t.Setenv("NM_DAEMON_CONNECT_TIMEOUT", timeout.String())

	originalDial := dialNetworkWithTimeout
	dialNetworkWithTimeout = func(network, address string, gotTimeout time.Duration) (net.Conn, error) {
		if gotTimeout != timeout {
			t.Fatalf("dial timeout = %v, want %v", gotTimeout, timeout)
		}
		time.Sleep(gotTimeout + 10*time.Millisecond)
		return nil, timeoutDialError{}
	}
	t.Cleanup(func() {
		dialNetworkWithTimeout = originalDial
	})

	socketPath := filepath.Join(shortTempDir(t), "dead.sock")
	if runtime.GOOS == "windows" {
		endpoint := fmt.Sprintf("127.0.0.1:1\ntoken\n%d", os.Getpid())
		if err := os.WriteFile(socketPath, []byte(endpoint), 0o600); err != nil {
			t.Fatalf("write endpoint file: %v", err)
		}
	}
	started := time.Now()
	client, err := Dial(socketPath)
	elapsed := time.Since(started)
	if client != nil {
		t.Fatal("Dial returned a client for a timed-out socket")
	}
	if err == nil {
		t.Fatal("Dial returned nil error for a timed-out socket")
	}
	if elapsed > 250*time.Millisecond {
		t.Fatalf("Dial took %v, want fast failure", elapsed)
	}
	if !IsConnectTimeout(err) {
		t.Fatalf("Dial error %T %v, want connect timeout", err, err)
	}
	if !strings.Contains(err.Error(), socketPath) {
		t.Fatalf("Dial error = %q, want socket path %q", err.Error(), socketPath)
	}
}

// TestDialUsesTheConnectTimeoutOfTheRootItDials pins the timeout to the root
// that owns the socket. A receive hook resolves its root from the gate it was
// handed and runs with no NM_HOME, so reading the ambient root's config would
// dial one daemon under another root's timeout.
func TestDialUsesTheConnectTimeoutOfTheRootItDials(t *testing.T) {
	writeRoot := func(root, timeout string) string {
		t.Helper()
		if err := os.MkdirAll(root, 0o755); err != nil {
			t.Fatal(err)
		}
		cfg := filepath.Join(root, "config.yaml")
		if err := os.WriteFile(cfg, []byte("daemon_connect_timeout: \""+timeout+"\"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		return filepath.Join(root, "socket")
	}

	base := shortTempDir(t)
	ambient := filepath.Join(base, "ambient")
	writeRoot(ambient, "1s")
	t.Setenv("NM_HOME", ambient)

	const want = 7 * time.Second
	socketPath := writeRoot(filepath.Join(base, "owning"), want.String())
	if runtime.GOOS == "windows" {
		endpoint := fmt.Sprintf("127.0.0.1:1\ntoken\n%d", os.Getpid())
		if err := os.WriteFile(socketPath, []byte(endpoint), 0o600); err != nil {
			t.Fatalf("write endpoint file: %v", err)
		}
	}

	var got time.Duration
	originalDial := dialNetworkWithTimeout
	dialNetworkWithTimeout = func(network, address string, timeout time.Duration) (net.Conn, error) {
		got = timeout
		return nil, timeoutDialError{}
	}
	t.Cleanup(func() { dialNetworkWithTimeout = originalDial })

	if _, err := Dial(socketPath); err == nil {
		t.Fatal("expected the dial to the dead socket to fail")
	}
	if got != want {
		t.Fatalf("dial timeout = %v, want %v (the owning root's, not the ambient root's)", got, want)
	}
}

func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "ipc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func TestDialWithDeadlineBoundsConnection(t *testing.T) {
	for _, tc := range []struct {
		name       string
		configured time.Duration
		budget     time.Duration
		wantMax    time.Duration
	}{
		{"startup budget", 5 * time.Second, 160 * time.Millisecond, 160 * time.Millisecond},
		{"configured timeout", 25 * time.Millisecond, 5 * time.Second, 25 * time.Millisecond},
		{"ordinary dial", 25 * time.Millisecond, 0, 25 * time.Millisecond},
		{"expired deadline", 5 * time.Second, -time.Second, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("NM_DAEMON_CONNECT_TIMEOUT", tc.configured.String())
			socket := filepath.Join(shortTempDir(t), "socket")
			if runtime.GOOS == "windows" {
				endpoint := fmt.Sprintf("127.0.0.1:1\ntoken\n%d", os.Getpid())
				if err := os.WriteFile(socket, []byte(endpoint), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			originalDial := dialNetworkWithTimeout
			calls := 0
			dialNetworkWithTimeout = func(network, address string, timeout time.Duration) (net.Conn, error) {
				calls++
				if timeout <= 0 || timeout > tc.wantMax {
					t.Fatalf("connection timeout = %v, want positive and at most %v", timeout, tc.wantMax)
				}
				if tc.budget == 0 || tc.budget > tc.configured {
					if timeout != tc.configured {
						t.Fatalf("connection timeout = %v, want configured %v", timeout, tc.configured)
					}
				}
				time.Sleep(timeout)
				return nil, timeoutDialError{}
			}
			t.Cleanup(func() { dialNetworkWithTimeout = originalDial })
			started := time.Now()
			var client *Client
			var err error
			if tc.budget == 0 {
				client, err = Dial(socket)
			} else {
				client, err = DialWithDeadline(socket, started.Add(tc.budget))
			}
			if client != nil || !IsConnectTimeout(err) || IsCallTimeout(err) {
				t.Fatalf("dial = %v, %v, want connection timeout only", client, err)
			}
			wantCalls := 1
			if tc.budget < 0 {
				wantCalls = 0
			}
			if calls != wantCalls {
				t.Fatalf("network dial calls = %d, want %d", calls, wantCalls)
			}
			if elapsed := time.Since(started); elapsed > 2500*time.Millisecond {
				t.Fatalf("dial took %v, want less than the uncapped 5s timeout", elapsed)
			}
		})
	}
}
