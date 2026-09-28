package app

import "github.com/wnsdy95/cxthub/cli/internal/ports/inbound"

func syncProgress(in inbound.SyncInput, operation, phase string, completed, total int) {
	if in.Progress != nil {
		in.Progress(inbound.SyncProgress{Operation: operation, Phase: phase, Completed: completed, Total: total})
	}
}
