//go:build darwin

package nativecodex

import "syscall"

type darwinProcessExitObserver struct{ fd int }

func newProcessExitObserver() (processExitObserver, error) {
	// Do not leak the kqueue into a concurrently spawned child between creation
	// and setting close-on-exec.
	syscall.ForkLock.RLock()
	fd, err := syscall.Kqueue()
	if err == nil {
		syscall.CloseOnExec(fd)
	}
	syscall.ForkLock.RUnlock()
	if err != nil {
		return nil, err
	}
	return &darwinProcessExitObserver{fd: fd}, nil
}

func (o *darwinProcessExitObserver) wait(pid int) error {
	change := syscall.Kevent_t{
		Ident: uint64(pid), Filter: syscall.EVFILT_PROC,
		Flags: syscall.EV_ADD | syscall.EV_ONESHOT, Fflags: syscall.NOTE_EXIT,
	}
	for {
		_, err := syscall.Kevent(o.fd, []syscall.Kevent_t{change}, nil, nil)
		if err == syscall.EINTR {
			continue
		}
		// An immediate exit may precede registration. No waiter has reaped
		// this child, so ESRCH cannot refer to a reused PID and Wait is safe.
		if err == syscall.ESRCH {
			return nil
		}
		if err != nil {
			return err
		}
		break
	}
	var events [1]syscall.Kevent_t
	for {
		n, err := syscall.Kevent(o.fd, nil, events[:], nil)
		if err == syscall.EINTR {
			continue
		}
		if err != nil {
			return err
		}
		if n == 0 {
			continue
		}
		event := events[0]
		if event.Flags&syscall.EV_ERROR != 0 {
			if event.Data == int64(syscall.ESRCH) {
				return nil
			}
			return syscall.Errno(event.Data)
		}
		if event.Ident == uint64(pid) && event.Filter == syscall.EVFILT_PROC && event.Fflags&syscall.NOTE_EXIT != 0 {
			return nil
		}
	}
}

func (o *darwinProcessExitObserver) close() { _ = syscall.Close(o.fd) }
