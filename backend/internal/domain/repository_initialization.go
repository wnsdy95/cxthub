package domain

import (
	"errors"
)

var ErrRepositoryInitializationUnsupported = errors.New("repository initialization requires transactional storage")
var ErrRepositoryInitializationConflict = errors.New("repository initialization conflicts with existing state")
