package memory

import (
	"strings"
	"unicode/utf8"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

const (
	managedMemoryBegin    = "<!-- cxt:begin managed memory (do not edit inside markers) -->"
	managedMemoryEnd      = "<!-- cxt:end managed memory -->"
	maxManagedMemoryBytes = 64 << 10
)

// renderMemoryMarkdown renders MemoryDigest to markdown for provider memory files (CLAUDE.md/AGENTS.md).
// Wrapped with cxt-managed markers to identify the region for merge/refresh.
// The immutable digest remains full fidelity in cxt storage; only this
// provider-facing projection is bounded so it cannot consume the next session's
// context window before the user says anything.
func renderMemoryMarkdown(d domain.MemoryDigest) string {
	d = domain.PromptStructuredProjection(d)
	header := managedMemoryBegin + "\n# cxt memory\n\n"
	footer := ""
	if d.SnapshotID != "" && domain.ValidateContentHash(d.SnapshotID) == nil {
		footer = "<!-- cxt:snapshot " + string(d.SnapshotID) + " -->\n"
	}
	footer += managedMemoryEnd + "\n"
	available := maxManagedMemoryBytes - len(header) - len(footer)
	if available < 0 {
		available = 0
	}

	// Structured state receives fixed reservations before narrative. Lists are
	// selected newest-first but rendered in their original order.
	facts := renderMemoryListTail("Key facts", d.KeyFacts, available/6)
	tasks := renderMemoryListTail("Open tasks", d.OpenTasks, available/4)
	remaining := available - len(facts) - len(tasks)
	summary := ""
	if text := strings.TrimSpace(d.Summary); text != "" && remaining > 2 {
		summary = truncateMemoryTail(text, remaining-2) + "\n\n"
	}
	return header + summary + facts + tasks + footer
}

func renderMemoryListTail(title string, items []string, maxBytes int) string {
	header := "## " + title + "\n"
	if maxBytes <= len(header)+3 || len(items) == 0 {
		return ""
	}
	remaining := maxBytes - len(header) - 1 // final section newline
	selected := make([]string, 0, len(items))
	for i := len(items) - 1; i >= 0; i-- {
		item := strings.TrimSpace(items[i])
		if item == "" {
			continue
		}
		line := "- " + item + "\n"
		if len(line) > remaining {
			if len(selected) == 0 && remaining > 3 {
				line = "- " + truncateMemoryPrefix(item, remaining-3) + "\n"
				selected = append(selected, line)
			}
			break
		}
		selected = append(selected, line)
		remaining -= len(line)
	}
	if len(selected) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(header)
	for i := len(selected) - 1; i >= 0; i-- {
		b.WriteString(selected[i])
	}
	b.WriteByte('\n')
	return b.String()
}

func truncateMemoryPrefix(text string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(text) <= maxBytes {
		return text
	}
	end := maxBytes
	for end > 0 && !utf8.ValidString(text[:end]) {
		end--
	}
	return text[:end]
}

func truncateMemoryTail(text string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(text) <= maxBytes {
		return text
	}
	const marker = "[... earlier memory omitted ...]\n"
	if maxBytes <= len(marker) {
		return truncateMemoryPrefix(marker, maxBytes)
	}
	start := len(text) - (maxBytes - len(marker))
	for start < len(text) && !utf8.RuneStart(text[start]) {
		start++
	}
	return marker + text[start:]
}
