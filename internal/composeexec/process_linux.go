//go:build linux

package composeexec

import (
	"os/exec"
	"syscall"
)

func platformSupported() error { return nil }

func configureProcessGroup(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func terminateProcessGroup(pid int) error { return syscall.Kill(-pid, syscall.SIGTERM) }
func killProcessGroup(pid int) error      { return syscall.Kill(-pid, syscall.SIGKILL) }
func isProcessDone(err error) bool        { return err == syscall.ESRCH }
