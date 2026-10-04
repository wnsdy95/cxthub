//go:build linux

package nativeclaude

import (
	"os"
	"syscall"
	"unsafe"
)

type linuxProcessExitObserver struct{}

func newProcessExitObserver() (processExitObserver, error) {
	return linuxProcessExitObserver{}, nil
}

func (linuxProcessExitObserver) wait(pid int) error {
	// waitid(P_PID, WEXITED|WNOWAIT) observes exit without releasing the PID.
	// This also works when Go cannot obtain a pidfd. syscall does not expose
	// Waitid, so supply the kernel's 128-byte siginfo_t buffer without decoding
	// its architecture-dependent fields. We only need the syscall result.
	var info [16]uint64
	const pPID = 1
	for {
		_, _, errno := syscall.Syscall6(syscall.SYS_WAITID, pPID, uintptr(pid), uintptr(unsafe.Pointer(&info)), syscall.WEXITED|syscall.WNOWAIT, 0, 0)
		if errno == syscall.EINTR {
			continue
		}
		if errno == syscall.ECHILD {
			return os.ErrProcessDone
		}
		if errno != 0 {
			return errno
		}
		return nil
	}
}

func (linuxProcessExitObserver) close() {}
