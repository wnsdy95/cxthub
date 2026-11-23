package domain

import (
	"errors"
)

var ErrDocFindDisplayUnavailable = errors.New("display text requires a prepared bounded projection")
