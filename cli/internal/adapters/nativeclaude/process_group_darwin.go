//go:build darwin

package nativeclaude

import (
	"encoding/binary"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	zombieGroupReadLimit = 25
	zombieGroupReadGap   = 2 * time.Millisecond
)

func processGroupContainsOnlyZombies(pid int) bool {
	// Raw performs a size query and one read. Unlike SysctlKinfoProcSlice it
	// does not retry forever if a live group keeps growing; ENOMEM is failure.
	return observeZombieProcessGroup(pid, func() ([]byte, error) {
		return unix.SysctlRaw("kern.proc.pgrp", pid)
	}, time.Sleep)
}

func observeZombieProcessGroup(pid int, snapshot func() ([]byte, error), pause func(time.Duration)) bool {
	if pid <= 1 {
		return false
	}
	for attempt := 0; attempt < zombieGroupReadLimit; attempt++ {
		raw, err := snapshot()
		if err != nil {
			return false
		}
		complete, pending := zombieProcessGroupState(raw, pid)
		if complete || !pending {
			return complete
		}
		// Darwin exit observation can precede SZOMB visibility. The caller
		// retains the unreaped leader throughout these bounded observations;
		// neither elapsed time nor a still-live leader is evidence of cleanup.
		if attempt+1 < zombieGroupReadLimit {
			pause(zombieGroupReadGap)
		}
	}
	return false
}

func zombieProcessGroup(raw []byte, pid int) bool {
	complete, _ := zombieProcessGroupState(raw, pid)
	return complete
}

func zombieProcessGroupState(raw []byte, pid int) (complete, pending bool) {
	if pid <= 1 || len(raw) == 0 || len(raw)%unix.SizeofKinfoProc != 0 {
		return false, false
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
		if member <= 1 || group != pid {
			return false, false
		}
		if member == pid {
			if foundLeader {
				return false, false
			}
			foundLeader = true
		}
		// SZOMB=5, SSLEEP=2, SRUN=3 in Darwin's sys/proc.h. Only the
		// owned leader's sleeping/running transition permits another read.
		// A live descendant, stopped/unknown state, or malformed snapshot
		// remains a failure, even if a later snapshot might omit it.
		if entry[stateOffset] != 5 {
			if member != pid || (entry[stateOffset] != 2 && entry[stateOffset] != 3) {
				return false, false
			}
			pending = true
		}
	}
	// An empty or filtered result is not proof of our unreaped child's
	// group. Its leader must be present in this kernel snapshot.
	return foundLeader && !pending, foundLeader && pending
}
