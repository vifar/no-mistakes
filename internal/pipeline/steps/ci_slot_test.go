package steps

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
)

type ciSlotRecorder struct {
	mu     sync.Mutex
	events []string
}

func (s *ciSlotRecorder) Yield() {
	s.mu.Lock()
	s.events = append(s.events, "yield")
	s.mu.Unlock()
}

func (s *ciSlotRecorder) Acquire(context.Context) error {
	s.mu.Lock()
	s.events = append(s.events, "acquire")
	s.mu.Unlock()
	return nil
}

func (s *ciSlotRecorder) log() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.events...)
}

// The CI step babysits a PR for as long as a week. The run must not hold a
// concurrency slot across that wait: it yields before each poll interval.
func TestCIStepYieldsRunSlotWhileWaitingForNextPoll(t *testing.T) {
	dir, base, head := setupGitRepo(t)
	api := newFakeBitbucketCIAPI(t, "OPEN", fmt.Sprintf(`{"values":[{"key":"test","name":"test","state":"INPROGRESS","links":{"commit":{"href":"https://api.bitbucket.org/2.0/repositories/test/repo/commit/%s"}}}]}`, head), head)
	c := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, base, head, config.Commands{})
	prURL := "https://bitbucket.org/test/repo/pull-requests/42"
	c.Run.PRURL = &prURL
	c.Repo.UpstreamURL = "https://bitbucket.org/test/repo.git"
	c.Env = fakeBitbucketEnv(api.server.URL)
	c.Config.CITimeout = -1
	slot := &ciSlotRecorder{}
	c.Slot = slot
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.Ctx = ctx

	var atWait []string
	ci := (&CIStep{}).SetWaitForNextPoll(func(ctx context.Context, _ time.Duration) error {
		atWait = slot.log()
		cancel()
		return ctx.Err()
	})
	if _, err := driveCI(t, ci, c); !errors.Is(err, context.Canceled) {
		t.Fatalf("driveCI error = %v, want cancellation from the first poll wait", err)
	}
	if len(atWait) != 1 || atWait[0] != "yield" {
		t.Fatalf("slot events when the poll wait began = %v, want exactly [yield]", atWait)
	}
}

func (*ciSlotRecorder) Reserve() {}
