package pipeline

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

const prContextReadUnavailable = "PR-context read unavailable after 3 attempts"

// PRContextReadTimedOut recognizes the durable error label after a run has
// stored its error chain as text. It affects diagnostic wording only, never
// retry eligibility or validation authority, which require the typed cause.
func PRContextReadTimedOut(message string) bool {
	return strings.Contains(message, prContextReadUnavailable+":") && strings.Contains(message, scm.ErrTransientTimeout.Error())
}

// ReadPRContext retries only a typed credential-store timeout. The callback
// must be a single read, never a step, agent turn, retarget or publication.
// Three attempts with 1s/2s backoff keep a transient keyring failure from
// throwing away completed review work without hiding real auth failures.
func ReadPRContext[T any](ctx context.Context, log func(string), read func() (T, error)) (T, error) {
	var value T
	for attempt := 1; attempt <= 3; attempt++ {
		if err := ctx.Err(); err != nil {
			return value, err
		}
		result, err := read()
		value = result
		if err == nil || !errors.Is(err, scm.ErrTransientTimeout) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return value, err
		}
		if attempt == 3 {
			return value, fmt.Errorf("%s: %w", prContextReadUnavailable, err)
		}
		delay := time.Duration(attempt) * time.Second
		if log != nil {
			log(fmt.Sprintf("PR-context read unavailable: transient keyring timeout; retry %d/3 in %s", attempt+1, delay))
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return value, ctx.Err()
		case <-timer.C:
		}
	}
	panic("unreachable")
}
