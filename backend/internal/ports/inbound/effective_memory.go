package inbound

import (
	"context"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// EffectiveMemoryQuery is shared by web and MCP. Adapters never infer code
// position, integration or memory validity from display graph placement.
type EffectiveMemoryQuery interface {
	QueryEffectiveMemory(context.Context, domain.ContentHash, domain.EffectiveMemoryRequest) (domain.EffectiveMemoryPage, error)
}
