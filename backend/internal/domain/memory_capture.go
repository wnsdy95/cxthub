package domain

import (
	"errors"
)

var ErrMemoryCaptureExpired = errors.New("memory capture retention expired")
