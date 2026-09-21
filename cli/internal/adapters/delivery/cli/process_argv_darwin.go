//go:build darwin

package cli

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"syscall"
	"unsafe"
)

// Read only the Git process named by the hook's parent PID. Never log raw argv;
// the caller immediately reduces it to non-secret branch creation arguments.
func readProcessArgv(pid int) ([]string, error) {
	mib := [3]int32{1, 49, int32(pid)} // CTL_KERN, KERN_PROCARGS2
	raw := make([]byte, 256<<10)
	size := uintptr(len(raw))
	_, _, errno := syscall.Syscall6(syscall.SYS___SYSCTL, uintptr(unsafe.Pointer(&mib[0])), 3, uintptr(unsafe.Pointer(&raw[0])), uintptr(unsafe.Pointer(&size)), 0, 0)
	if errno != 0 {
		return nil, errno
	}
	if size < 5 || size > uintptr(len(raw)) {
		return nil, fmt.Errorf("invalid process arguments")
	}
	raw = raw[:size]
	argc := int(binary.NativeEndian.Uint32(raw[:4]))
	raw = raw[4:]
	if argc < 1 || argc > 4096 {
		return nil, fmt.Errorf("invalid process argument count")
	}
	end := bytes.IndexByte(raw, 0)
	if end < 0 {
		return nil, fmt.Errorf("missing executable")
	}
	raw = raw[end+1:]
	for len(raw) > 0 && raw[0] == 0 {
		raw = raw[1:]
	}
	result := make([]string, 0, argc)
	for len(result) < argc {
		end = bytes.IndexByte(raw, 0)
		if end < 0 {
			return nil, fmt.Errorf("truncated arguments")
		}
		result = append(result, string(raw[:end]))
		raw = raw[end+1:]
	}
	return result, nil
}
