package domain

import (
	"testing"
)

type legacyNativeFloat64DifferentialCounts struct {
	fast     int
	decimal  int
	accepted int
	rejected int
}

func (counts legacyNativeFloat64DifferentialCounts) report(test *testing.T) {
	test.Helper()
	test.Logf("native differential outcomes: fast=%d decimal=%d accepted=%d rejected=%d", counts.fast, counts.decimal, counts.accepted, counts.rejected)
}
