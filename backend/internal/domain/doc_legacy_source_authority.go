package domain

import "sync/atomic"

var legacyNormalizationSourceSequence atomic.Uint64

func newLegacyNormalizationSourceAuthority() (uint64, error) {
	for {
		previous := legacyNormalizationSourceSequence.Load()
		if previous == ^uint64(0) {
			return 0, ErrIntegrity
		}
		if legacyNormalizationSourceSequence.CompareAndSwap(previous, previous+1) {
			return previous + 1, nil
		}
	}
}
