package cli

import (
	"fmt"
	"io"

	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
)

func syncProgressPrinter(w io.Writer) func(inbound.SyncProgress) {
	var previous inbound.SyncProgress
	return func(p inbound.SyncProgress) {
		if p == previous {
			return
		}
		previous = p
		label := map[string]string{
			"prepare":                       "preparing repository and retained state",
			"negotiate":                     "checking which objects the server needs",
			"verify-memory":                 "verifying memory dependencies",
			"upload-and-verify-documents":   "uploading and verifying documents",
			"download-and-verify-documents": "downloading and verifying documents",
			"verify-history-and-memory":     "verifying history and memory",
			"store-verified-objects":        "storing verified objects",
			"publish-snapshots":             "recording snapshots",
			"recover-prerequisites":         "recovering concurrently removed prerequisites",
			"memory-and-history":            "publishing memory and history",
			"publish-memory":                "publishing memory attachments",
			"publish-lineage":               "recording queued messages and grafts",
			"publish-history":               "recording context history",
			"publish-refs":                  "publishing branch and retained-history refs",
			"adopt-refs":                    "reconciling local refs",
			"reconcile-pending":             "reconciling pending sessions",
			"conflicts":                     "conflicts retained for inspection",
			"complete":                      "foreground synchronization complete",
		}[p.Phase]
		if label == "" {
			return
		}
		if p.Total > 0 {
			fmt.Fprintf(w, "%s: %s (%d/%d)\n", p.Operation, label, p.Completed, p.Total)
		} else {
			fmt.Fprintf(w, "%s: %s\n", p.Operation, label)
		}
	}
}
