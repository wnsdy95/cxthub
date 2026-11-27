package domain

import (
	"errors"
)

var ErrStagingVersion = errors.New("unsupported staging version")
var ErrEmptyIndex = errors.New("no frozen sessions staged; run cxt add first")
