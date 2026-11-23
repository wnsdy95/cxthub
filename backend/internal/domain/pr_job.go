package domain

import (
	"errors"
)

var ErrPROrderingPending = errors.New("PR merge ordering evidence is not available")
