package daemon

import "testing"

func TestProcessRunningRejectsNonProcessPIDs(t *testing.T) {
	for _, pid := range []int{0, -1} {
		if running, err := processRunning(pid); err != nil || running {
			t.Fatalf("pid %d: running=%v err=%v", pid, running, err)
		}
	}
}
