//go:build !windows

package pipeline

import "testing"

func TestParseProcessCPUTime(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  uint64
	}{
		{name: "darwin zero", value: "0:00.00", want: 0},
		{name: "fractional", value: "1:02.50", want: 62_500_000_000},
		{name: "hours", value: "1:02:03.50", want: 3_723_500_000_000},
		{name: "malformed", value: "0:00.", want: ^uint64(0)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := parseProcessCPUTime(tt.value); got != tt.want {
				t.Fatalf("parseProcessCPUTime(%q) = %d, want %d", tt.value, got, tt.want)
			}
		})
	}
}
