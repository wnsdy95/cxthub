//go:build darwin

package nativeclaude

import (
	"encoding/binary"
	"unsafe"

	"golang.org/x/sys/unix"
)

func processGroupContainsOnlyZombies(pid int) bool {
	// Raw performs a size query and one read. Unlike SysctlKinfoProcSlice it
	// does not retry forever if a live group keeps growing; ENOMEM is failure.
	raw, err := unix.SysctlRaw("kern.proc.pgrp", pid)
	return err == nil && zombieProcessGroup(raw, pid)
}

func zombieProcessGroup(raw []byte, pid int) bool {
	if pid <= 1 || len(raw) == 0 || len(raw)%unix.SizeofKinfoProc != 0 {
		return false
	}
	// Use the OS adapter's native layouts, reading only integer fields. Do
	// not copy kernel pointer fields into Go objects or retain process data.
	var info unix.KinfoProc
	pidOffset := int(unsafe.Offsetof(info.Proc) + unsafe.Offsetof(info.Proc.P_pid))
	stateOffset := int(unsafe.Offsetof(info.Proc) + unsafe.Offsetof(info.Proc.P_stat))
	groupOffset := int(unsafe.Offsetof(info.Eproc) + unsafe.Offsetof(info.Eproc.Pgid))
	foundLeader := false
	for offset := 0; offset < len(raw); offset += unix.SizeofKinfoProc {
		entry := raw[offset : offset+unix.SizeofKinfoProc]
		member := int(int32(binary.NativeEndian.Uint32(entry[pidOffset:])))
		group := int(int32(binary.NativeEndian.Uint32(entry[groupOffset:])))
		// SZOMB is 5 in Darwin's sys/proc.h. Sleeping, stopped, embryonic
		// and unknown states are not evidence of completed termination.
		if member <= 1 || group != pid || entry[stateOffset] != 5 {
			return false
		}
		if member == pid {
			if foundLeader {
				return false
			}
			foundLeader = true
		}
	}
	// An empty or filtered result is not proof of our unreaped child's
	// group. Its leader must be present in this kernel snapshot.
	return foundLeader
}
