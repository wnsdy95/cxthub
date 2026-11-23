package domain

import (
	"errors"
)

var ErrBranchPullUnsupported = errors.New("branch pull planning unsupported by this storage adapter")
