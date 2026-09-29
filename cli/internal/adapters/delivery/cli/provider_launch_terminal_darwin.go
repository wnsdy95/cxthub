//go:build darwin

package cli

import (
	"os"
	"runtime"
	"syscall"
	"unsafe"
)

func providerLaunchTerminal(file *os.File) bool {
	var term syscall.Termios
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, file.Fd(), syscall.TIOCGETA, uintptr(unsafe.Pointer(&term)))
	runtime.KeepAlive(file)
	return errno == 0
}
