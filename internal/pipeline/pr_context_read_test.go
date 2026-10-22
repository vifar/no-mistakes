package pipeline

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

func TestIssue100PRContextRetryIsBoundedAndBacksOff(t *testing.T) {
	var starts []time.Time
	var logs []string
	_, err := ReadPRContext(context.Background(), func(s string) { logs = append(logs, s) }, func() (string, error) {
		starts = append(starts, time.Now())
		return "", fmt.Errorf("gh read: %w", scm.ErrTransientTimeout)
	})
	if len(starts) != 3 || !errors.Is(err, scm.ErrTransientTimeout) || !strings.Contains(err.Error(), "after 3 attempts") {
		t.Fatalf("attempts=%d err=%v", len(starts), err)
	}
	if starts[1].Sub(starts[0]) < time.Second || starts[2].Sub(starts[1]) < 2*time.Second {
		t.Fatalf("backoff missing: %v", starts)
	}
	if len(logs) != 2 || !strings.Contains(logs[0], "transient keyring timeout") {
		t.Fatalf("logs=%v", logs)
	}
}

func TestIssue100PRContextReadDoesNotRetryAuthOrCancellation(t *testing.T) {
	for _, failure := range []error{scm.ErrUnauthenticated, context.Canceled, context.DeadlineExceeded, errors.New("HTTP 401: Bad credentials"), errors.New("unknown forge error")} {
		calls := 0
		_, err := ReadPRContext(context.Background(), nil, func() (string, error) { calls++; return "", failure })
		if calls != 1 || !errors.Is(err, failure) {
			t.Fatalf("calls=%d err=%v want=%v", calls, err, failure)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	_, err := ReadPRContext(ctx, func(string) { cancel() }, func() (string, error) { calls++; return "", scm.ErrTransientTimeout })
	if calls != 1 || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation during backoff: calls=%d err=%v", calls, err)
	}
}
