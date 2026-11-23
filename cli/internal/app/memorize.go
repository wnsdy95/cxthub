package app

import (
	"strings"

	"github.com/wnsdy95/cxthub-cli/internal/domain"
)

// memoryCarryBudgetBytes bounds the summary carried forward by a merged digest
// (#33 — bounded carry, user-approved policy). MergeDigests concatenates
// generations oldest-first, so the forward working set grew without bound
// (measured 768KB in the dogfood repo). The carried copy keeps the newest
// tail; every prior generation's full digest object stays attached to its
// ancestor snapshot (content-addressed, never deleted), so complete history
// remains recoverable through the parent chain.
const memoryCarryBudgetBytes = 256 << 10
const memoryCarryListBudgetBytes = 64 << 10

// boundCarriedDigest caps only an inherited digest before it is merged into a
// fresh generation. The fresh digest itself is never truncated: otherwise a
// >256KiB native memory or newly generated summary would be lossy on its first
// storage and there would be no full ancestor object to recover it from.
func boundCarriedDigest(d domain.MemoryDigest) domain.MemoryDigest {
	// A carried digest becomes future provider context. Keep immutable archive
	// objects untouched, but do not copy legacy tool facts or unattested task
	// unions into another active generation.
	d = domain.HistoricalPromptProjection(d)
	// Legacy memories can contain recursively materialized cxt seed summaries.
	// They remain immutable on their ancestor snapshots, but must not be copied
	// into the active projection again. Structured facts/tasks are retained and
	// the fresh snapshot contributes a clean extractive/provider summary.
	if hasNestedSeedSummary(d.Summary) {
		d.Summary = ""
	}
	if len(d.Summary) > memoryCarryBudgetBytes {
		d.Summary = truncateUTF8Tail(d.Summary, memoryCarryBudgetBytes)
	}
	d.KeyFacts = boundStringListTail(d.KeyFacts, memoryCarryListBudgetBytes)
	d.OpenTasks = boundStringListTail(d.OpenTasks, memoryCarryListBudgetBytes)
	if len(d.Fragments) > 0 {
		remainingSummary := memoryCarryBudgetBytes
		remainingFacts := memoryCarryListBudgetBytes
		remainingTasks := memoryCarryListBudgetBytes
		kept := make([]domain.MemoryFragment, 0, len(d.Fragments))
		for i := len(d.Fragments) - 1; i >= 0; i-- {
			fragment := d.Fragments[i]
			if hasNestedSeedSummary(fragment.Summary) {
				fragment.Summary = ""
			}
			fragment.Summary = truncateUTF8Tail(fragment.Summary, remainingSummary)
			fragment.KeyFacts = boundStringListTail(fragment.KeyFacts, remainingFacts)
			fragment.OpenTasks = boundStringListTail(fragment.OpenTasks, remainingTasks)
			if fragment.Summary == "" && len(fragment.KeyFacts) == 0 && len(fragment.OpenTasks) == 0 && !fragment.TasksAuthoritative && len(fragment.Claims) == 0 {
				continue
			}
			remainingSummary -= len(fragment.Summary)
			remainingFacts -= stringListBytes(fragment.KeyFacts)
			remainingTasks -= stringListBytes(fragment.OpenTasks)
			kept = append(kept, fragment)
		}
		for left, right := 0, len(kept)-1; left < right; left, right = left+1, right-1 {
			kept[left], kept[right] = kept[right], kept[left]
		}
		d.Fragments = kept
	}
	return d
}

func hasNestedSeedSummary(summary string) bool {
	return strings.Contains(summary, seedSummaryPrefix) ||
		strings.Contains(summary, "[cxt seed] Branch-switch context:")
}

func stringListBytes(items []string) int {
	total := 0
	for _, item := range items {
		total += len(item)
	}
	return total
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
