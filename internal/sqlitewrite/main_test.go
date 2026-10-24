package sqlitewrite

import (
	"os"
	"testing"
	"time"
)

// Contention tests hold real locks past the budget. Keep that wall time at the
// former 30 seconds; tests of the production budget use DefaultBudget.
func TestMain(m *testing.M) {
	Budget = 30 * time.Second
	os.Exit(m.Run())
}
