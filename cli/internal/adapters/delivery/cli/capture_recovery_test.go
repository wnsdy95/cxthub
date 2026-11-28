package cli

import (
	"testing"
)

func TestCaptureRecoveryCommandPreflight(t *testing.T) {
	for _, args := range [][]string{{"capture"}, {"capture", "show"}, {"capture", "resolve", "id"}, {"capture", "acknowledge", "id", "--expect", "hash"}, {"capture", "retry", "id", "--expect", "hash", "--reason", "no"}, {"capture", "list", "--expect", "hash"}, {"capture", "unknown"}} {
		if _, err := PreflightArgs(append([]string{"cxt"}, args...)); err == nil {
			t.Fatalf("accepted invalid command: %v", args)
		}
	}
}
