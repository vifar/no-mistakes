//go:build e2e

package daemon

import (
	"os"
	"path/filepath"
	"time"
)

// Only the e2e binary can deschedule an answer handler at this boundary.
// The normal binary has no environment switch or filesystem barrier.
func init() {
	dir := os.Getenv("NM_E2E_ANSWER_BARRIER")
	if dir == "" {
		return
	}
	defaultBeforeAnswerRespond = func() {
		if err := os.WriteFile(filepath.Join(dir, "arrived"), nil, 0o600); err != nil {
			panic(err)
		}
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(filepath.Join(dir, "release")); err == nil {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		panic("e2e answer barrier was not released")
	}
}
