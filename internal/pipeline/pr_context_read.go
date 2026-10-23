package pipeline

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/netretry"
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
// throwing away completed review work without hiding real auth failures. It
// shares the network retry helper's bounded, cancellation-aware loop; the
// credential class is typed here rather than matched from text.
func ReadPRContext[T any](ctx context.Context, log func(string), read func() (T, error)) (T, error) {
	return netretry.Do(ctx, netretry.Options{
		Operation: "PR-context read",
		Attempts:  3,
		Base:      time.Second,
		Max:       2 * time.Second,
		Log:       log,
		Retryable: func(err error) bool {
			return errors.Is(err, scm.ErrTransientTimeout) &&
				!errors.Is(err, context.Canceled) &&
				!errors.Is(err, context.DeadlineExceeded)
		},
		Describe: func(error) string { return scm.ErrTransientTimeout.Error() },
		Exhausted: func(_ int, err error) error {
			return fmt.Errorf("%s: %w", prContextReadUnavailable, err)
		},
	}, read)
}
