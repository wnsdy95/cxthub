package domain

import (
	"errors"
)

var ErrDocVisibleProjectionUnsupported = errors.New("visible document projection unsupported")
var ErrDocVisibleGenerationStale = errors.New("visible document generation changed")
