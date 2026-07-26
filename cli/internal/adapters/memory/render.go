package memory

import (
	"strings"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

// renderMemoryMarkdown renders MemoryDigest to markdown for provider memory files (CLAUDE.md/AGENTS.md).
// Wrapped with cxt-managed marker to identify the region for merge/refresh.
func renderMemoryMarkdown(d domain.MemoryDigest) string {
	var b strings.Builder
	b.WriteString("<!-- cxt:begin managed memory (do not edit inside markers) -->\n")
	b.WriteString("# cxt memory\n\n")
	if d.Summary != "" {
		b.WriteString(d.Summary)
		b.WriteString("\n\n")
	}
	if len(d.KeyFacts) > 0 {
		b.WriteString("## Key facts\n")
		for _, f := range d.KeyFacts {
			b.WriteString("- " + f + "\n")
		}
		b.WriteString("\n")
	}
	if len(d.OpenTasks) > 0 {
		b.WriteString("## Open tasks\n")
		for _, t := range d.OpenTasks {
			b.WriteString("- " + t + "\n")
		}
		b.WriteString("\n")
	}
	if d.SnapshotID != "" {
		b.WriteString("<!-- cxt:snapshot " + string(d.SnapshotID) + " -->\n")
	}
	b.WriteString("<!-- cxt:end managed memory -->\n")
	return b.String()
}
