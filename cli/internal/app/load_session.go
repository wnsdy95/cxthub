package app

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/wnsdy95/cxthub-cli/internal/domain"
)

func materializationCwd(cwd string) string {
	if cwd == "" {
		return ""
	}
	abs, err := filepath.Abs(cwd)
	if err != nil {
		return cwd
	}
	return abs
}

// semantic maximum (Codex profile; mandatory opaque state may overflow it)

func seedBudgets(provider domain.ProviderKind) (total, digest int) {
	switch provider {
	case domain.ProviderCodex:
		return 288 << 10, 72 << 10 // ~96k estimated input tokens
	case domain.ProviderClaude:
		return 192 << 10, 48 << 10 // ~64k estimated input tokens
	default:
		return 192 << 10, 48 << 10
	}
}

func eventsJSONBytes(evs []domain.Event) int {
	total := 0
	for _, ev := range evs {
		b, _ := json.Marshal(ev)
		total += len(b)
	}
	return total
}

func isSeedSummaryEvent(ev domain.Event) bool {
	return ev.Kind == domain.EventMessage && ev.Role == "user" && len(ev.Blocks) > 0 &&
		strings.HasPrefix(ev.Blocks[0].Text, seedSummaryPrefix)
}

func isSeedUserBoundary(ev domain.Event) bool {
	return ev.Kind == domain.EventMessage && ev.Role == "user" && !isSeedSummaryEvent(ev)
}

// trimEventsForSeed keeps a bounded recent tail and returns the exact omitted
// events. Normally the cut advances to the next user message, preserving whole
// turns. If the newest in-progress turn alone exceeds the budget, there is no
// next user boundary; in that case the latest user request is anchored in
// front of the bounded suffix. Unmatched tool results at that synthetic gap are
// removed so the provider never receives an output without its call.
func trimEventsForSeed(cir domain.CIRDocument, budget int) (domain.CIRDocument, []domain.Event) {
	return trimEventsForSeedWithKnownCalls(cir, budget, nil)
}

func trimEventsForSeedWithKnownCalls(cir domain.CIRDocument, budget int, knownCalls map[string]bool) (domain.CIRDocument, []domain.Event) {
	return trimEventsForSeedWithPinned(cir, budget, knownCalls, nil)
}

func trimEventsForSeedWithPinned(cir domain.CIRDocument, budget int, knownCalls map[string]bool, pinned map[int]bool) (domain.CIRDocument, []domain.Event) {
	evs := cir.Events
	if eventsJSONBytes(evs) <= budget {
		return cir, nil
	}
	// Pinned state is mandatory provider replay state, not semantic transcript.
	// Charging it against this allowance can reduce the semantic budget to zero
	// and split the newest turn even though that turn itself fits the configured
	// replay budget.
	semanticBudget := budget
	sum := 0
	cut := len(evs)
	for i := len(evs) - 1; i >= 0; i-- {
		if pinned[i] {
			continue
		}
		b, _ := json.Marshal(evs[i])
		if sum+len(b) > semanticBudget {
			break
		}
		sum += len(b)
		cut = i
	}

	selected := make([]bool, len(evs))
	nextUser := -1
	for i := cut; i < len(evs); i++ {
		if isSeedUserBoundary(evs[i]) {
			nextUser = i
			break
		}
	}

	if nextUser >= 0 {
		for i := nextUser; i < len(evs); i++ {
			selected[i] = true
		}
	} else {
		latestUser := -1
		for i := cut - 1; i >= 0; i-- {
			if isSeedUserBoundary(evs[i]) {
				latestUser = i
				break
			}
		}
		if latestUser < 0 {
			for i := cut; i < len(evs); i++ {
				selected[i] = true
			}
		} else {
			selected[latestUser] = true
			userBytes, _ := json.Marshal(evs[latestUser])
			remaining := semanticBudget - len(userBytes)
			suffixCut := len(evs)
			suffixBytes := 0
			if remaining > 0 {
				for i := len(evs) - 1; i > latestUser; i-- {
					if pinned[i] {
						continue
					}
					b, _ := json.Marshal(evs[i])
					if suffixBytes+len(b) > remaining {
						break
					}
					suffixBytes += len(b)
					suffixCut = i
				}
			}
			for i := suffixCut; i < len(evs); i++ {
				selected[i] = true
			}
		}
	}
	for i := range pinned {
		if i >= 0 && i < len(selected) {
			selected[i] = true
		}
	}

	// Any bounded suffix can begin after a tool call but before its result.
	// Keep only results whose call is also present in the selected stream.
	calls := make(map[string]bool, len(knownCalls))
	for callID := range knownCalls {
		calls[callID] = true
	}
	for i, keep := range selected {
		if keep && evs[i].Kind == domain.EventToolCall && evs[i].CallID != "" {
			calls[evs[i].CallID] = true
		}
	}
	for i, keep := range selected {
		if keep && evs[i].Kind == domain.EventToolResult && evs[i].CallID != "" && !calls[evs[i].CallID] {
			selected[i] = false
		}
	}

	out := cir
	out.Events = make([]domain.Event, 0, len(evs))
	omitted := make([]domain.Event, 0, len(evs))
	for i, ev := range evs {
		if selected[i] {
			out.Events = append(out.Events, ev)
		} else {
			omitted = append(omitted, ev)
		}
	}
	if len(omitted) == 0 {
		return cir, nil
	}
	return out, omitted
}

func truncateUTF8Prefix(s string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(s) <= maxBytes {
		return s
	}
	end := maxBytes
	for end > 0 && !utf8.ValidString(s[:end]) {
		end--
	}
	return s[:end]
}

func truncateUTF8Tail(s string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(s) <= maxBytes {
		return s
	}
	const marker = "[... earlier summary omitted ...]\n"
	if maxBytes <= len(marker) {
		return truncateUTF8Prefix(marker, maxBytes)
	}
	start := len(s) - (maxBytes - len(marker))
	for start < len(s) && !utf8.RuneStart(s[start]) {
		start++
	}
	return marker + s[start:]
}

func renderSeedBulletSection(title string, items []string, maxBytes int) string {
	header := "\n" + title + "\n"
	if len(items) == 0 || maxBytes <= len(header)+2 {
		return ""
	}
	var b strings.Builder
	b.WriteString(header)
	for _, item := range items {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		line := "- " + item + "\n"
		remaining := maxBytes - b.Len()
		if remaining <= 2 {
			break
		}
		if len(line) > remaining {
			b.WriteString("- " + truncateUTF8Prefix(item, remaining-2))
			break
		}
		b.WriteString(line)
	}
	if b.Len() == len(header) {
		return ""
	}
	return b.String()
}

func renderSeedDigestWithHeader(digest domain.MemoryDigest, header string, budget int) string {
	digest = domain.HistoricalPromptProjection(digest)
	if len(header) >= budget {
		return truncateUTF8Prefix(header, budget)
	}

	facts := renderSeedBulletSection("Key facts:", seedWorthyFacts(digest.KeyFacts), budget/6)
	taskHeading := seedLegacyTasksHeading
	if digest.TasksAuthoritative {
		taskHeading = seedAuthoritativeTasksHeading
	}
	tasks := renderSeedBulletSection(taskHeading, digest.OpenTasks, budget/4)
	if digest.TasksAuthoritative && tasks == "" && budget/4 > len(taskHeading) {
		// Preserve an explicit all-tasks-completed tombstone if this prompt is
		// later the sole recoverable cxt seed. Unmarked legacy headings are never
		// upgraded to authority.
		tasks = taskHeading + "\n"
	}
	remaining := budget - len(header) - len(facts) - len(tasks)

	summaryBlock := ""
	if summary := strings.TrimSpace(digest.Summary); summary != "" && remaining > 2 {
		summary = truncateUTF8Tail(summary, remaining-2)
		summaryBlock = "\n" + summary + "\n"
	}
	out := header + summaryBlock + facts + tasks
	if len(out) > budget {
		return truncateUTF8Prefix(out, budget)
	}
	return out
}

// `seedSummaryPrefix` is the identifier prefix for seed compression summary events — a marker to find and replace (remove) the previous generation summary in the tail when the next seed is generated. It is removed from materialized copies, while the saved doc remains unchanged.
const seedSummaryPrefix = "[cxt] This session was resumed from a branch context seed."

const (
	seedLegacyTasksHeading        = "Open tasks:"
	seedAuthoritativeTasksHeading = "Open tasks: <!-- cxt:tasks-authoritative-v1 -->"
)

// seedWorthyFacts retains only KeyFacts that are worth placing in the seed header. The distillation now
// extracts sentence-form facts from compressed summaries, but legacy stored digests may contain tool-name lists and ingestion markers (empirically observed: "apply_patch", "unknown:Agent", "native memory: …") —
// excluding whitespace-free single tokens and marker prefixes, only sentence-form facts pass.
func seedWorthyFacts(facts []string) []string {
	return domain.PromptWorthyMemoryFacts(facts)
}
