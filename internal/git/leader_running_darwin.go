package git

import "golang.org/x/sys/unix"

func commandLeaderRunning(pid int) bool {
	const zombie = 5
	const exiting = 0x00002000
	info, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	return err == nil && info != nil && info.Proc.P_pid == int32(pid) && info.Proc.P_stat > 0 && info.Proc.P_stat < zombie && info.Proc.P_flag&exiting == 0
}
