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
			out.OpenTasks = nil
		}
		return out
	}

	out.Fragments = make([]MemoryFragment, len(d.Fragments))
	out.TasksAuthoritative = false
	factSet := make(map[string]struct{})
	for i, source := range d.Fragments {
		fragment := source
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
	renderMemoryFragments(&out)
	return out
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
