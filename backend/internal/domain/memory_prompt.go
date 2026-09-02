package domain

import "strings"

// PromptStructuredProjection returns a provider-visible copy of a memory
// digest without mutating its immutable archival representation. Summaries
// retain project history, but cumulative provider generations are collapsed,
// transport metadata is removed from facts, and only provider-attested task
// state can appear as instructions to a future agent.
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
// embedded verbatim by a provider. A single occurrence is retained exactly so
// arbitrary prose that merely resembles a continuation is never truncated.
func LatestProviderCompactionGeneration(summary string) string {
	last, occurrences := -1, 0
	for offset := 0; offset < len(summary); {
		end := strings.IndexByte(summary[offset:], '\n')
		if end < 0 {
			end = len(summary)
		} else {
			end += offset
		}
		if isProviderContinuationHeader(strings.TrimSpace(summary[offset:end])) {
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
	switch strings.TrimPrefix(line, providerContinuationPrefix) {
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
	return strings.ToLower(strings.Trim(strings.TrimSpace(line[i+2:]), "*#: ")), true
}

func isTaskSectionTitle(title string) bool {
	return title == "pending tasks" || title == "open tasks" ||
		strings.HasPrefix(title, "pending tasks ") || strings.HasPrefix(title, "open tasks ")
}

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

// PromptWorthyMemoryFacts filters transport/provenance labels from facts that
// are about to become provider-controlled context. Stored digests are intact.
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

// IsNativeMemoryProvenanceFact identifies source labels emitted by older cxt
// distillers. These are archive metadata rather than project knowledge.
func IsNativeMemoryProvenanceFact(fact string) bool {
	fact = strings.ToLower(strings.TrimSpace(fact))
	return strings.HasPrefix(fact, "native memory:") ||
		strings.HasPrefix(fact, "absorbed from") ||
		strings.HasPrefix(fact, "ingested from")
}

const (
	extractiveFallbackHeader           = "Conversation digest (extractive fallback; provider compaction summary unavailable):"
	extractiveFallbackUsersHeader      = "Recent user intent:"
	extractiveFallbackAssistantsHeader = "Recent assistant outcomes:"
	extractiveFallbackDeltaMarker      = "[cxt conversation delta v1]"
)

func renderExtractiveFallbackSummary(users, assistants []string) string {
	var b strings.Builder
	b.WriteString(extractiveFallbackHeader)
	b.WriteByte('\n')
	writeExtractiveFallbackSection(&b, extractiveFallbackUsersHeader, users)
	writeExtractiveFallbackSection(&b, extractiveFallbackAssistantsHeader, assistants)
	return strings.TrimSpace(b.String())
}

func writeExtractiveFallbackSection(b *strings.Builder, header string, items []string) {
	if len(items) == 0 {
		return
	}
	b.WriteString("\n\n")
	b.WriteString(header)
	b.WriteByte('\n')
	for _, item := range items {
		if item = strings.TrimSpace(item); item != "" {
			b.WriteString("- ")
			b.WriteString(item)
			b.WriteByte('\n')
		}
	}
}

func parseExtractiveFallbackSummary(summary string) (users, assistants []string, ok bool) {
	lines := strings.Split(strings.TrimSpace(summary), "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != extractiveFallbackHeader {
		return nil, nil, false
	}
	section := 0
	seenUsers, seenAssistants := false, false
	for _, raw := range lines[1:] {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		switch line {
		case extractiveFallbackUsersHeader:
			if seenUsers || seenAssistants {
				return nil, nil, false
			}
			seenUsers, section = true, 1
		case extractiveFallbackAssistantsHeader:
			if seenAssistants {
				return nil, nil, false
			}
			seenAssistants, section = true, 2
		default:
			if section == 0 || !strings.HasPrefix(line, "- ") {
				return nil, nil, false
			}
			item := strings.TrimSpace(strings.TrimPrefix(line, "- "))
			if item == "" {
				return nil, nil, false
			}
			if section == 1 {
				users = append(users, item)
			} else {
				assistants = append(assistants, item)
			}
		}
	}
	if len(users) == 0 && len(assistants) == 0 {
		return nil, nil, false
	}
	return users, assistants, true
}

func splitExtractiveFallbackSummary(summary string) (baseline string, users, assistants []string, ok bool) {
	summary = strings.TrimSpace(summary)
	if users, assistants, ok = parseExtractiveFallbackSummary(summary); ok {
		return "", users, assistants, true
	}
	marker := "\n\n" + extractiveFallbackDeltaMarker + "\n"
	if at := strings.LastIndex(summary, marker); at >= 0 {
		candidate := summary[at+len(marker):]
		if users, assistants, ok = parseExtractiveFallbackSummary(candidate); ok {
			return strings.TrimSpace(summary[:at]), users, assistants, true
		}
	}
	return summary, nil, nil, false
}

func renderMemoryFragments(out *MemoryDigest) {
	var summaries []string
	fallbackInsertAt := -1
	var fallbackUsers, fallbackAssistants []string
	var facts, tasks []string
	for _, fragment := range out.Fragments {
		if summary := strings.TrimSpace(fragment.Summary); summary != "" {
			baseline, users, assistants, hasFallback := splitExtractiveFallbackSummary(summary)
			if baseline != "" && !containsString(summaries, baseline) {
				summaries = append(summaries, baseline)
			}
			if hasFallback {
				fallbackInsertAt = len(summaries)
				fallbackUsers = dedupStrings(fallbackUsers, users)
				fallbackAssistants = dedupStrings(fallbackAssistants, assistants)
			} else if baseline == "" && !containsString(summaries, summary) {
				summaries = append(summaries, summary)
			}
		}
		facts = dedupStrings(facts, fragment.KeyFacts)
		if fragment.TasksAuthoritative {
			tasks = dedupStrings(fragment.OpenTasks)
		} else {
			tasks = dedupStrings(tasks, fragment.OpenTasks)
		}
	}
	if fallbackInsertAt >= 0 {
		summaries = append(summaries, "")
		copy(summaries[fallbackInsertAt+1:], summaries[fallbackInsertAt:])
		summaries[fallbackInsertAt] = renderExtractiveFallbackSummary(fallbackUsers, fallbackAssistants)
	}
	out.Summary = strings.Join(summaries, "\n\n")
	out.KeyFacts = facts
	out.OpenTasks = tasks
}

func containsString(items []string, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}

func dedupStrings(lists ...[]string) []string {
	seen := map[string]bool{}
	var out []string
	for _, list := range lists {
		for _, item := range list {
			if item == "" || seen[item] {
				continue
			}
			seen[item] = true
			out = append(out, item)
		}
	}
	return out
}
