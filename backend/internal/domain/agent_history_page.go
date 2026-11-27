package domain

import (
	"errors"
)

var ErrAgentHistoryUnavailable = errors.New("verified history read index unavailable")
