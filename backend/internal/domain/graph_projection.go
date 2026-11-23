package domain

import "errors"

const GraphProjectionRules = 1

var ErrGraphProjectionNotReady = errors.New("graph projection is not ready for the current source")

var ErrGraphProjectionFailed = errors.New("graph projection requires attention")
