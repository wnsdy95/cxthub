package mcp

import (
	"context"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

// Direct core-service fixture calls explicitly run as trusted internal work.
func systemTestContext() context.Context { return inbound.WithSystemActor(context.Background()) }
