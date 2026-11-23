package domain

import (
	"fmt"
)

var ErrJoinPreviewChanged = fmt.Errorf("%w: join preview changed; review the current plan before confirming", ErrConflict)
