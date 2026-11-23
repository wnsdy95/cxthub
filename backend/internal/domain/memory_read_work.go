package domain

import (
	"errors"
)

var ErrMemoryReadNotReady = errors.New("memory read model is being prepared")
var ErrMemoryReadNeedsAttention = errors.New("memory read preparation requires attention")
