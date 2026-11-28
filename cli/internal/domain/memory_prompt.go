package domain

import "strings"

// PromptStructuredProjection returns the provider-visible structured state of
// a memory digest without mutating it.
//
// The summary stays lossless apart from task sections. Facts are filtered to
// project knowledge, while tasks are emitted only from a provider-attested
// structured extraction. Other task lists, and the task sections of the
// summary narrative, cannot become instructions in a branch summary or a fork
// seed.
func PromptStructuredProjection(d MemoryDigest) MemoryDigest {
	out := d
	out.KeyFacts = PromptWorthyMemoryFacts(d.KeyFacts)
	if d.TasksAuthoritative {
		out.OpenTasks = promptWorthyMemoryTasks(d.OpenTasks, memoryItemSet(out.KeyFacts))
	} else {
		out.Summary = withoutStructuredTaskNarrative(out.Summary)
		out.OpenTasks = nil
	}
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

const CodeAssessmentBegin = "<!-- cxt:code-assessment:v1 -->"
const CodeAssessmentEnd = "<!-- cxt:end-code-assessment -->"

// WithoutCodeAssessment removes only complete generated assessment blocks from
// prompt copies. Stored digests/transcripts remain immutable. A prior code
// selection's status must be queried afresh, never recursively carried as fact.
func WithoutCodeAssessment(text string) string {
	for offset := 0; offset < len(text); {
		start := strings.Index(text[offset:], CodeAssessmentBegin)
		if start < 0 {
			break
		}
		start += offset
		end := strings.Index(text[start+len(CodeAssessmentBegin):], CodeAssessmentEnd)
		if end < 0 {
			break
		}
		end += start + len(CodeAssessmentBegin) + len(CodeAssessmentEnd)
		text = text[:start] + text[end:]
		offset = start
	}
	return text
}

// HistoricalPromptProjection removes obsolete code assessments, then applies
// PromptStructuredProjection, when inherited history is prepared for a tool.
func HistoricalPromptProjection(d MemoryDigest) MemoryDigest {
	out := d
	out.Summary = WithoutCodeAssessment(d.Summary)
	return PromptStructuredProjection(out)
}
