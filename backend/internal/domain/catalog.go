package domain

import (
	"errors"
)

var (
	ErrCatalogUnsupported = errors.New("catalog synchronization unsupported by this storage adapter")
)
