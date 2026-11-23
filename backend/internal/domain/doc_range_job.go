package domain

import (
	"errors"
)

var ErrDocRangeIndexPending = errors.New("document range index is being prepared")
