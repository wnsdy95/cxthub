package app

import (
	"strings"

	"github.com/wnsdy95/cxthub-cli/internal/domain"
)

// memoryCarryBudgetBytes bounds the summary text carried into a tool's
// context (#33 — bounded carry, user-approved policy). The carried copy keeps
// the newest tail; the summary the server keeps for its commit is not changed.
const memoryCarryBudgetBytes = 256 << 10
const memoryCarryListBudgetBytes = 64 << 10

// boundCarriedDigest caps a summary read back from the server before a tool is
// given it, in a branch summary or a fork seed. What is uploaded is never
// changed here; only this carried copy is bounded.
func boundCarriedDigest(d domain.MemoryDigest) domain.MemoryDigest {
	// A carried digest becomes provider context: legacy tool facts and
	// unattested task lists are not given to the tool.
	d = domain.HistoricalPromptProjection(d)
	// A summary can contain a cxt seed an earlier session was given. It is not
	// given again; structured facts and tasks are kept.
	if hasNestedSeedSummary(d.Summary) {
		d.Summary = ""
	}
	if len(d.Summary) > memoryCarryBudgetBytes {
		d.Summary = truncateUTF8Tail(d.Summary, memoryCarryBudgetBytes)
	}
	d.KeyFacts = boundStringListTail(d.KeyFacts, memoryCarryListBudgetBytes)
	d.OpenTasks = boundStringListTail(d.OpenTasks, memoryCarryListBudgetBytes)
	return d
}

func hasNestedSeedSummary(summary string) bool {
	return strings.Contains(summary, seedSummaryPrefix) ||
		strings.Contains(summary, "[cxt seed] Branch-switch context:")
}

func boundStringListTail(items []string, maxBytes int) []string {
	if maxBytes <= 0 || len(items) == 0 {
		return nil
	}
	used := 0
	start := len(items)
	for start > 0 {
		n := len(items[start-1])
		if used+n > maxBytes {
			break
		}
		used += n
		start--
	}
	if start == 0 {
		return items
	}
	if start == len(items) {
		return []string{truncateUTF8Tail(items[len(items)-1], maxBytes)}
	}
	return append([]string(nil), items[start:]...)
}
