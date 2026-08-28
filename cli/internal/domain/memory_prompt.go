package domain

import "strings"

// PromptStructuredProjection returns the provider-visible structured state of
// a memory digest without mutating its immutable archival representation.
//
// Summaries remain lossless and provenance fragments remain addressable. Facts
// are filtered to project knowledge, while tasks are emitted only from a
// provider-attested structured extraction. Extractive/legacy task lists are
// retained in storage for inspection but cannot become instructions in a
// future load, branch seed, managed memory file, or carried generation.
func PromptStructuredProjection(d MemoryDigest) MemoryDigest {
	out := d
	if len(d.Fragments) == 0 {
		out.KeyFacts = PromptWorthyMemoryFacts(d.KeyFacts)
		if d.TasksAuthoritative {
			out.OpenTasks = promptWorthyMemoryTasks(d.OpenTasks, memoryItemSet(out.KeyFacts))
		} else {
			out.Summary = withoutStructuredTaskNarrative(out.Summary)
			out.OpenTasks = nil
		}
		return out
	}

	out.Fragments = make([]MemoryFragment, len(d.Fragments))
	out.TasksAuthoritative = false
	latestTaskAuthority := -1
	for i, source := range d.Fragments {
		if source.TasksAuthoritative {
			latestTaskAuthority = i
		}
	}
	factSet := make(map[string]struct{})
	for i, source := range d.Fragments {
		fragment := source
		fragment.Summary = LatestProviderCompactionGeneration(source.Summary)
		if i != latestTaskAuthority {
			fragment.Summary = withoutStructuredTaskNarrative(fragment.Summary)
		}
		fragment.KeyFacts = PromptWorthyMemoryFacts(source.KeyFacts)
		fragment.OpenTasks = nil
		out.Fragments[i] = fragment
		for key := range memoryItemSet(fragment.KeyFacts) {
			factSet[key] = struct{}{}
		}
	}
	for i, source := range d.Fragments {
		if source.TasksAuthoritative {
			out.TasksAuthoritative = true
			out.Fragments[i].OpenTasks = promptWorthyMemoryTasks(source.OpenTasks, factSet)
		}
	}
	dropByteContainedProviderSummaries(out.Fragments)
	renderMemoryFragments(&out)
	return out
}

const providerContinuationPrefix = "This session is being continued from a previous conversation"

// LatestProviderCompactionGeneration removes older cumulative generations
// that some providers embed verbatim inside the next compaction summary. The
// raw CIR remains immutable; this canonical form is used only for distilled or
// provider-visible memory. A single occurrence is left byte-for-byte intact so
// ordinary prose that merely uses the heading cannot be truncated.
func LatestProviderCompactionGeneration(summary string) string {
	last, occurrences := -1, 0
	for offset := 0; offset < len(summary); {
		end := strings.IndexByte(summary[offset:], '\n')
		if end < 0 {
			end = len(summary)
		} else {
			end += offset
		}
		line := strings.TrimSpace(summary[offset:end])
		if isProviderContinuationHeader(line) {
			last = offset
			occurrences++
		}
		if end == len(summary) {
			break
		}
		offset = end + 1
	}
	if occurrences < 2 || last < 0 {
		return summary
	}
	return strings.TrimSpace(summary[last:])
}

func isProviderContinuationHeader(line string) bool {
	if !strings.HasPrefix(line, providerContinuationPrefix) {
		return false
	}
	rest := strings.TrimPrefix(line, providerContinuationPrefix)
	switch rest {
	case ".",
		" that ran out of context.",
		" that ran out of context. The summary below covers the earlier portion of the conversation.":
		return true
	default:
		return false
	}
}

func withoutStructuredTaskNarrative(summary string) string {
	if summary == "" {
		return ""
	}
	lines := strings.SplitAfter(summary, "\n")
	out := make([]string, 0, len(lines))
	skipping := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		title, numbered := numberedMemorySectionTitle(line)
		if numbered {
			if isTaskSectionTitle(title) {
				skipping = true
				continue
			}
			if skipping {
				skipping = false
			}
		} else if skipping && (trimmed == extractiveFallbackDeltaMarker ||
			trimmed == extractiveFallbackHeader || isProviderContinuationHeader(trimmed)) {
			skipping = false
		}
		if !skipping {
			out = append(out, line)
		}
	}
	return strings.TrimSpace(strings.Join(out, ""))
}

func numberedMemorySectionTitle(line string) (string, bool) {
	line = strings.TrimRight(line, "\r\n")
	trimmedLeft := strings.TrimLeft(line, " ")
	if len(line)-len(trimmedLeft) > 3 {
		return "", false
	}
	line = strings.TrimSpace(trimmedLeft)
	if line == "" || line[0] < '1' || line[0] > '9' {
		return "", false
	}
	i := 1
	for i < len(line) && line[i] >= '0' && line[i] <= '9' {
		i++
	}
	if i+1 >= len(line) || line[i] != '.' || line[i+1] != ' ' {
		return "", false
	}
	title := strings.ToLower(strings.Trim(strings.TrimSpace(line[i+2:]), "*#: "))
	return title, true
}

func isTaskSectionTitle(title string) bool {
	return title == "pending tasks" || title == "open tasks" ||
		strings.HasPrefix(title, "pending tasks ") || strings.HasPrefix(title, "open tasks ")
}

// dropByteContainedProviderSummaries removes only canonical provider
// continuation summaries whose complete bytes are represented by another
// fragment. Unique sibling summaries remain independent; equal copies prefer
// the later fragment for deterministic recency.
func dropByteContainedProviderSummaries(fragments []MemoryFragment) {
	for i := range fragments {
		candidate := strings.TrimSpace(fragments[i].Summary)
		if candidate == "" || !isCanonicalProviderContinuation(candidate) {
			continue
		}
		for j := range fragments {
			if i == j {
				continue
			}
			other := strings.TrimSpace(fragments[j].Summary)
			if len(other) < len(candidate) || !isCanonicalProviderContinuation(other) {
				continue
			}
			if strings.Contains(other, candidate) && (len(other) > len(candidate) || j > i) {
				fragments[i].Summary = ""
				break
			}
		}
	}
}

func isCanonicalProviderContinuation(summary string) bool {
	if end := strings.IndexByte(summary, '\n'); end >= 0 {
		summary = summary[:end]
	}
	return isProviderContinuationHeader(strings.TrimSpace(summary))
}

// PromptWorthyMemoryFacts is the shared filter for structured project facts
// rendered into provider-controlled context. Older distillers stored tool
// tokens and source-provenance labels alongside facts; those remain in the
// archive but are not useful project knowledge.
func PromptWorthyMemoryFacts(facts []string) []string {
	out := make([]string, 0, len(facts))
	seen := make(map[string]struct{}, len(facts))
	for _, fact := range facts {
		item := strings.TrimSpace(fact)
		if !promptWorthyMemoryItem(item) {
			continue
		}
		key := strings.ToLower(item)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, item)
	}
	return out
}

func promptWorthyMemoryTasks(tasks []string, facts map[string]struct{}) []string {
	out := make([]string, 0, len(tasks))
	seen := make(map[string]struct{}, len(tasks))
	for _, task := range tasks {
		item := strings.TrimSpace(task)
		if !promptWorthyMemoryItem(item) {
			continue
		}
		key := strings.ToLower(item)
		if _, duplicateFact := facts[key]; duplicateFact {
			continue
		}
		if _, duplicateTask := seen[key]; duplicateTask {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, item)
	}
	return out
}

func memoryItemSet(items []string) map[string]struct{} {
	out := make(map[string]struct{}, len(items))
	for _, item := range items {
		out[strings.ToLower(strings.TrimSpace(item))] = struct{}{}
	}
	return out
}

func promptWorthyMemoryItem(item string) bool {
	if item == "" || !strings.ContainsAny(item, " \t\n") || IsNativeMemoryProvenanceFact(item) {
		return false
	}
	lower := strings.ToLower(item)
	for _, prefix := range []string{
		"[cxt] this session was resumed from a branch context seed.",
		"[cxt seed] branch-switch context:",
		"<environment_context",
		"<turn_aborted",
		"conversation digest (extractive fallback;",
		"recent user intent:",
		"recent assistant outcomes:",
		"session summary:",
		"tools [",
	} {
		if strings.HasPrefix(lower, prefix) {
			return false
		}
	}
	return true
}
