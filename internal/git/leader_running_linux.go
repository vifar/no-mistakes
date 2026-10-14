package git

import "golang.org/x/sys/unix"

func commandLeaderRunning(pid int) bool {
	var info unix.Siginfo
	return unix.Waitid(unix.P_PID, pid, &info, unix.WEXITED|unix.WNOHANG|unix.WNOWAIT, nil) == nil && info.Code == 0
}
