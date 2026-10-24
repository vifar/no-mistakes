//go:build e2e

package sqlitewrite

import "time"

// E2e journeys hold real locks until a write exhausts its budget. Only the e2e
// binary keeps the former 30-second budget so those journeys stay bounded.
func init() { Budget = 30 * time.Second }
