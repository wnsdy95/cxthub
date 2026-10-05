//go:build darwin || linux

package nativeclaude

import (
	"errors"
	"os/exec"
	"syscall"
)

// cleanupProcessGroup makes the final group termination request before the
// sole waiter reaps the leader. A successful signal means the kernel accepted
// it, not that descendants which escaped this group are covered. Callers must
// still join the leader and their readers, even when this request fails.
func cleanupProcessGroup(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil || cmd.Process.Pid <= 1 || cmd.ProcessState != nil {
		return ErrCleanup
	}
	err := signalProcessGroup(cmd, true)
	if err == nil || errors.Is(err, syscall.ESRCH) {
		return nil
	}
	// Darwin excludes zombies from killpg and can return EPERM when the
	// unreaped leader is the last member. Never excuse EPERM just because
	// the leader exited: a live descendant may still be in the same group.
	if errors.Is(err, syscall.EPERM) && processGroupContainsOnlyZombies(cmd.Process.Pid) {
		return nil
	}
	return ErrCleanup
}
