package domain

import (
	"errors"
)

var (
	ErrCatalogUnsupported   = errors.New("catalog synchronization unsupported by this storage adapter")
	ErrCatalogResetRequired = errors.New("catalog synchronization requires a new baseline")
)
