//go:build darwin || linux

package nativecodex

import (
	"os/exec"
	"syscall"
)

func localSocketSupported() bool { return true }
func signalOwnedProcess(cmd *exec.Cmd, force bool) error {
	// Caller holds the lifecycle mutex and has checked that reaping has not
	// begun. Go's Process alone does not prevent a wait/signal race on Darwin.
	if force {
		return cmd.Process.Kill()
	}
	return cmd.Process.Signal(syscall.SIGTERM)
}

func ownedProcessAlive(cmd *exec.Cmd) bool { return cmd.Process.Signal(syscall.Signal(0)) == nil }
