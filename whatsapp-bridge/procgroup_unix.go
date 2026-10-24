//go:build unix

package main

import (
	"os/exec"
	"syscall"
)

// configureProcessGroup runs the command in its own process group and kills the whole
// group on timeout, so a draft command cannot leave children (such as claude) running.
func configureProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
