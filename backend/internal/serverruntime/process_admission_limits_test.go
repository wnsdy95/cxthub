package serverruntime

import (
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func TestProcessAdmissionLimitsExtendBulkWaitOnlyForAPI(t *testing.T) {
	api := processAdmissionLimits(outbound.ProcessAPI, 2, 4)
	if api.Active != 2 || api.Queue != 4 || api.BulkWait != app.ResourceBulkWaitLimit {
		t.Fatalf("API admission limits: %+v", api)
	}
	for _, role := range []outbound.ProcessRole{outbound.ProcessMCP, outbound.ProcessWorker} {
		if limits := processAdmissionLimits(role, 1, 4); limits.BulkWait != 0 || limits.Active != 1 || limits.Queue != 4 {
			t.Fatalf("%s admission changed: %+v", role, limits)
		}
	}
}
