package domain

import "errors"

var ErrResourceOverloaded = errors.New("resource capacity unavailable; retry later")
