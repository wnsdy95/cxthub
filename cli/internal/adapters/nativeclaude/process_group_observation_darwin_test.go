//go:build darwin

package nativeclaude

import (
	"encoding/binary"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

func zombieObservationEntry(pid, group int, state byte) []byte {
	var info unix.KinfoProc
	raw := make([]byte, unix.SizeofKinfoProc)
	binary.NativeEndian.PutUint32(raw[unsafe.Offsetof(info.Proc)+unsafe.Offsetof(info.Proc.P_pid):], uint32(pid))
	binary.NativeEndian.PutUint32(raw[unsafe.Offsetof(info.Eproc)+unsafe.Offsetof(info.Eproc.Pgid):], uint32(group))
	raw[unsafe.Offsetof(info.Proc)+unsafe.Offsetof(info.Proc.P_stat)] = state
	return raw
}

func TestDarwinZombieObservationWaitsForPositiveTransition(t *testing.T) {
	const leader = 12345
	for _, tc := range []struct {
		name       string
		zombieRead int
		want       bool
		reads      int
		pauses     int
	}{
		{"already zombie", 1, true, 1, 0},
		{"exiting then zombie", 3, true, 3, 2},
		{"last bounded snapshot", 25, true, 25, 24},
		{"transition beyond bound", 26, false, 25, 24},
		{"denied live leader", 0, false, 25, 24},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			reads, pauses := 0, 0
			var elapsed time.Duration
			got := observeZombieProcessGroup(leader, func() ([]byte, error) {
				reads++
				state := byte(2 + (reads-1)%2) // SSLEEP and SRUN are not proof.
				if tc.zombieRead > 0 && reads >= tc.zombieRead {
					state = 5
				}
				return zombieObservationEntry(leader, leader, state), nil
			}, func(gap time.Duration) {
				if gap != 2*time.Millisecond {
					t.Fatalf("observation gap = %v", gap)
				}
				pauses++
				elapsed += gap
			})
			if got != tc.want || reads != tc.reads || pauses != tc.pauses || elapsed > 48*time.Millisecond {
				t.Fatalf("proof=%t reads=%d pauses=%d elapsed=%v; want proof=%t reads=%d pauses=%d", got, reads, pauses, elapsed, tc.want, tc.reads, tc.pauses)
			}
		})
	}
}

func TestDarwinZombieObservationRejectsUncertainOrDeniedGroup(t *testing.T) {
	const leader = 12345
	zombie := zombieObservationEntry(leader, leader, 5)
	group := func(state byte) []byte {
		return append(zombieObservationEntry(leader, leader, 2), zombieObservationEntry(leader+1, leader, state)...)
	}
	for _, tc := range []struct {
		name string
		raw  []byte
		err  error
	}{
		{"denied snapshot", zombie, syscall.EPERM},
		{"growing snapshot", zombie, syscall.ENOMEM},
		{"empty", nil, nil},
		{"truncated", zombie[:len(zombie)-1], nil},
		{"missing leader", zombieObservationEntry(leader+1, leader, 5), nil},
		{"foreign group", zombieObservationEntry(leader, leader+1, 5), nil},
		{"duplicate leader", append(zombieObservationEntry(leader, leader, 2), zombie...), nil},
		{"invalid member", append(zombieObservationEntry(leader, leader, 2), zombieObservationEntry(0, leader, 5)...), nil},
		{"unknown leader", zombieObservationEntry(leader, leader, 0), nil},
		{"embryonic leader", zombieObservationEntry(leader, leader, 1), nil},
		{"stopped leader", zombieObservationEntry(leader, leader, 4), nil},
		{"sleeping descendant", group(2), nil},
		{"running descendant", group(3), nil},
		{"stopped descendant", group(4), nil},
		{"unknown descendant", group(0), nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			reads, pauses := 0, 0
			got := observeZombieProcessGroup(leader, func() ([]byte, error) {
				reads++
				if reads == 1 {
					return tc.raw, tc.err
				}
				// A later filtered snapshot must not hide uncertain evidence or
				// the previously observed live descendant.
				return zombie, nil
			}, func(time.Duration) { pauses++ })
			if got || reads != 1 || pauses != 0 {
				t.Fatalf("uncertain group accepted/retried: proof=%t reads=%d pauses=%d", got, reads, pauses)
			}
		})
	}
	for _, pid := range []int{-1, 0, 1} {
		if observeZombieProcessGroup(pid, func() ([]byte, error) {
			t.Fatal("invalid group queried")
			return zombie, nil
		}, func(time.Duration) { t.Fatal("invalid group waited") }) {
			t.Fatal("invalid group accepted")
		}
	}
}
