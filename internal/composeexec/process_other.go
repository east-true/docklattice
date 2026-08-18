//go:build !linux

package composeexec

import (
	"os/exec"
)

func platformSupported() error        { return ErrUnsupportedPlatform }
func configureProcessGroup(*exec.Cmd) {}
func terminateProcessGroup(int) error { return ErrUnsupportedPlatform }
func killProcessGroup(int) error      { return ErrUnsupportedPlatform }
func isProcessDone(error) bool        { return false }
