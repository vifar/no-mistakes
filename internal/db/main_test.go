package db

import (
	"os"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/sqlitewrite"
)

// Contention tests hold real locks past the budget. Keep that wall time at the
// former 30 seconds instead of the production sqlitewrite.DefaultBudget.
func TestMain(m *testing.M) {
	sqlitewrite.Budget = 30 * time.Second
	os.Exit(m.Run())
}
