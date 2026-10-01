//go:build darwin

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
		// Darwin sys/un.h: SOL_LOCAL=0, LOCAL_PEERPID=0x002.
		pid, peerErr = syscall.GetsockoptInt(int(fd), 0, 0x002)
	})
	if err != nil {
		return 0, err
	}
	return pid, peerErr
}
