package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCIReadinessReceiptTimeoutConfig(t *testing.T) {
	for _, tc := range []struct {
		value   string
		want    time.Duration
		invalid bool
	}{
		{"", 10 * time.Minute, false}, {"1h", time.Hour, false}, {"0s", 0, true}, {"-1m", 0, true}, {"unlimited", 0, true}, {"bad", 0, true},
	} {
		t.Run(tc.value, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			text := "{}\n"
			if tc.value != "" {
				text = "ci_readiness_receipt_timeout: " + tc.value + "\n"
			}
			if err := os.WriteFile(path, []byte(text), 0600); err != nil {
				t.Fatal(err)
			}
			global, err := LoadGlobal(path)
			if tc.invalid {
				if err == nil {
					t.Fatal("accepted an unbounded or invalid receipt budget")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			// Assert the effective value consumed by the monitor, through the real parser and merge.
			cfg := Merge(global, &RepoConfig{})
			if cfg.CIReadinessReceiptTimeout != tc.want {
				t.Fatalf("budget=%v want %v", cfg.CIReadinessReceiptTimeout, tc.want)
			}
		})
	}
}

func TestCIReadinessReceiptTimeoutShippedDefault(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	EnsureDefaultGlobalConfig(path)
	global, err := LoadGlobal(path)
	if err != nil {
		t.Fatal(err)
	}
	if global.CIReadinessReceiptTimeout != 10*time.Minute {
		t.Fatalf("shipped default=%v", global.CIReadinessReceiptTimeout)
	}
}
