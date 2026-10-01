//go:build linux

package nativecodex

import (
	"net"
	"syscall"
)

func peerProcess(conn net.Conn) (int, error) {
	unixConn, ok := conn.(*net.UnixConn)
	if !ok {
		return 0, ErrProtocol
	}
	raw, err := unixConn.SyscallConn()
	if err != nil {
		return 0, err
	}
	var pid int
	var peerErr error
	err = raw.Control(func(fd uintptr) {
		var credential *syscall.Ucred
		credential, peerErr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
		if peerErr == nil {
			pid = int(credential.Pid)
		}
	})
	if err != nil {
		return 0, err
	}
	return pid, peerErr
}
