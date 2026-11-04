package documentquery

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

func finishDocumentResponse(ctx context.Context, release func(), err error) {
	if err != nil || !inbound.RetainDocumentResponse(ctx, release) {
		release()
	}
}
