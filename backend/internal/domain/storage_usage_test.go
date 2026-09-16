package domain

import (
	"testing"
	"time"
)

func TestStoragePolicyAndRecovery(t *testing.T) {
	p := StoragePolicy{Plan: "enterprise", IncludedBytes: 50 * GiB, PayAsYouGo: true}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	p.IncludedBytes--
	if p.Validate() == nil {
		t.Fatal("wrong enterprise allowance accepted")
	}
	p.IncludedBytes = 50 * GiB
	now := time.Now()
	cap := 55 * GiB
	p.MaxBytes = &cap
	future := now.Add(time.Hour)
	p.GraceUntil = &future
	p.GraceBytes = 2 * GiB
	for _, tc := range []struct {
		bytes int64
		at    time.Time
		state string
	}{{44 * GiB, now, "active"}, {49 * GiB, now, "warning"}, {51 * GiB, now, "overage"}, {56 * GiB, now, "grace"}, {56 * GiB, future, "read_only"}, {58 * GiB, now, "read_only"}, {44 * GiB, future, "active"}} {
		u := StorageUsage{Policy: p, CurrentBytes: tc.bytes}
		u.ProjectState(tc.at)
		if u.State != tc.state {
			t.Fatalf("%d => %s want %s", tc.bytes, u.State, tc.state)
		}
	}
}
