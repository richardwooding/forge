//go:build unix

package build

import (
	"os/exec"
	"syscall"
)

// setProcessGroup puts the build in its own process group.
//
// go build spawns compiler and linker subprocesses. Killing only the go command
// leaves those running, so a cancelled or timed-out build would keep consuming
// the machine after forge had given up on it.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func killGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	// A negative pid addresses the whole group.
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
		return cmd.Process.Kill()
	}
	return nil
}
