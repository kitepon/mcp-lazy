//go:build !linux && !darwin

package main

import (
	"os"
	"os/exec"
)

func shutdownSignals() []os.Signal { return []os.Signal{os.Interrupt} }

// Group cleanup is supported on Linux and macOS only. Other platforms are untested.
func configureProcess(cmd *exec.Cmd) {}
func terminateProcessGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}
func killProcessGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}
