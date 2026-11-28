package storage

import (
	"github.com/wnsdy95/cxthub-cli/internal/ports/outbound"
)

var _ outbound.MetadataCheckpointStore = (*FileStore)(nil)
