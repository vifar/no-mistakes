//go:build !darwin && !linux

package git

func commandLeaderRunning(int) bool { return false }
