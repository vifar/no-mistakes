//go:build !windows

package pipeline

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

var sampleAgentProcess = sampleAgentProcessOS

func sampleAgentProcessOS(pid int) (uint64, string, error) {
	if pid <= 0 {
		return 0, "", fmt.Errorf("invalid process id %d", pid)
	}
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "time=,state=").Output()
	if err != nil {
		return 0, "", err
	}
	fields := strings.Fields(string(out))
	if len(fields) < 2 {
		return 0, "", fmt.Errorf("invalid process sample %q", strings.TrimSpace(string(out)))
	}
	return parseProcessCPUTime(fields[0]), fields[1], nil
}

func parseProcessCPUTime(value string) uint64 {
	parts := strings.Split(value, ":")
	if len(parts) != 2 && len(parts) != 3 {
		return ^uint64(0)
	}
	var totalSeconds uint64
	var fractionNanos uint64
	for i, part := range parts {
		if i == len(parts)-1 {
			fraction := strings.Split(part, ".")
			if len(fraction) > 2 || fraction[0] == "" || (len(fraction) == 2 && fraction[1] == "") || (len(fraction) == 2 && len(fraction[1]) > 9) {
				return ^uint64(0)
			}
			part = fraction[0]
			if len(fraction) == 2 {
				var err error
				fractionNanos, err = strconv.ParseUint(fraction[1]+strings.Repeat("0", 9-len(fraction[1])), 10, 32)
				if err != nil {
					return ^uint64(0)
				}
			}
		}
		v, err := strconv.ParseUint(part, 10, 32)
		if err != nil || totalSeconds > (^uint64(0)-v)/60 {
			return ^uint64(0)
		}
		totalSeconds = totalSeconds*60 + v
	}
	if totalSeconds > (^uint64(0)-fractionNanos)/1_000_000_000 {
		return ^uint64(0)
	}
	return totalSeconds*1_000_000_000 + fractionNanos
}
