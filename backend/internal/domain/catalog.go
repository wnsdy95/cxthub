package domain

import (
	"errors"
)

var (
	ErrCatalogUnsupported   = errors.New("catalog synchronization unsupported by this storage adapter")
	ErrCatalogResetRequired = errors.New("catalog synchronization requires a new baseline")
)

func catalogEpochValid(epoch string) bool {
	if len(epoch) != 36 {
		return false
	}
	for i := range len(epoch) {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if epoch[i] != '-' {
				return false
			}
			continue
		}
		c := epoch[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}
