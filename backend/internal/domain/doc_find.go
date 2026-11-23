package domain

import (
	"errors"
)

var ErrDocFindFieldsUnavailable = errors.New("bounded searchable fields are unavailable for this event")

var ErrDocFindFieldsPending = errors.New("bounded searchable fields are being prepared")
var ErrDocDisplayPending = errors.New("displayed event pages are being prepared")
