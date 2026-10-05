//go:build darwin

package nativeclaude

import (
	"encoding/binary"
	"os/exec"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

func TestDarwinZombieProofForUnreapedOwnedLeader(t *testing.T) {
	observer, err := newProcessExitObserver()
	if err != nil {
		t.Fatal(err)
	}
	defer observer.close()
	cmd := exec.Command("/bin/sh", "-c", "exit 0")
	configureProcess(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	observed := make(chan error, 1)
	go func() { observed <- observer.wait(cmd.Process.Pid) }()
	select {
	case err = <-observed:
	case <-time.After(3 * time.Second):
		_ = signalProcessGroup(cmd, true)
		_ = cmd.Wait()
		t.Fatal("owned leader exit was not observed")
	}
	if err != nil {
		_ = cmd.Wait()
		t.Fatal(err)
	}
	proven := processGroupContainsOnlyZombies(cmd.Process.Pid)
	cleanupErr := cleanupProcessGroup(cmd)
	waitErr := cmd.Wait() // Sole reap, after every group lookup/signal.
	if !proven || cleanupErr != nil || waitErr != nil {
		t.Fatalf("zombie proof=%t cleanup=%v wait=%v", proven, cleanupErr, waitErr)
	}
}

func TestZombieProcessGroupRequiresCompletePositiveEvidence(t *testing.T) {
	const leader = 12345
	entry := func(pid, group int, state byte) []byte {
		var info unix.KinfoProc
		raw := make([]byte, unix.SizeofKinfoProc)
		binary.NativeEndian.PutUint32(raw[unsafe.Offsetof(info.Proc)+unsafe.Offsetof(info.Proc.P_pid):], uint32(pid))
		binary.NativeEndian.PutUint32(raw[unsafe.Offsetof(info.Eproc)+unsafe.Offsetof(info.Eproc.Pgid):], uint32(group))
		raw[unsafe.Offsetof(info.Proc)+unsafe.Offsetof(info.Proc.P_stat)] = state
		return raw
	}
	zombie := entry(leader, leader, 5)
	for _, tc := range []struct {
		name string
		raw  []byte
		want bool
	}{
		{"leader zombie", zombie, true},
		{"all zombies", append(append([]byte{}, zombie...), entry(leader+1, leader, 5)...), true},
		{"empty", nil, false},
		{"truncated", zombie[:len(zombie)-1], false},
		{"leader absent", entry(leader+1, leader, 5), false},
		{"leader alive", entry(leader, leader, 2), false},
		{"leader stopped", entry(leader, leader, 4), false},
		{"live descendant", append(append([]byte{}, zombie...), entry(leader+1, leader, 2)...), false},
		{"unknown descendant", append(append([]byte{}, zombie...), entry(leader+1, leader, 0)...), false},
		{"wrong group", entry(leader, leader+1, 5), false},
		{"invalid pid", append(append([]byte{}, zombie...), entry(0, leader, 5)...), false},
		{"duplicate leader", append(append([]byte{}, zombie...), zombie...), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := zombieProcessGroup(tc.raw, leader); got != tc.want {
				t.Fatalf("positive zombie group evidence = %t, want %t", got, tc.want)
			}
		})
	}
	for _, pid := range []int{-1, 0, 1} {
		if zombieProcessGroup(zombie, pid) {
			t.Fatalf("invalid group %d accepted", pid)
		}
	}
}
