package memory

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

// RuleDistiller is the deterministic, rule-based MemoryDistiller implementation (compatibility rules).
//
// Native-first with fallback: if native is non-nil, ingest its content; otherwise derive summaries and facts from CIR. The same (cir, native) pair always produces the same digest.
type RuleDistiller struct{}

// NewRuleDistiller creates a RuleDistiller.
func NewRuleDistiller() *RuleDistiller { return &RuleDistiller{} }

// Distill generates a MemoryDigest from CIR(+native). Deterministic.
//
// Summary source priority:
//  1. The agent's latest context-compression summary (Claude isCompactSummary / Codex compacted.message). These summaries are cumulative and provide the highest-quality source.
//  2. Native memory ingestion (memory file created by the provider CLI).
//  3. Rules-based self-distillation (statistical summary — fallback).
func (d *RuleDistiller) Distill(_ context.Context, cir domain.CIRDocument, native *domain.NativeMemory) (domain.MemoryDigest, error) {
	prov := cir.Envelope.SourceProvider
	compactSummary := ""
	var userMsgs, asstMsgs int
	firstUser := ""
	toolSet := map[string]struct{}{}
	for _, ev := range cir.Events {
		switch ev.Kind {
		case domain.EventMessage:
			if ev.CompactSummary && len(ev.Blocks) > 0 && ev.Blocks[0].Text != "" &&
				// cxt-synthesized seed digests carry the CompactSummary marking but
				// are bounded copies of inherited memory, not the agent's cumulative
				// compression — promoting one here would source memorize from
				// boilerplate and mislabel it as agent-written (#38).
				!containsSyntheticSeedText(ev.Blocks[0].Text) {
				compactSummary = ev.Blocks[0].Text // last wins: newest cumulative summary
			}
			if ev.Role == "user" {
				userMsgs++
				if firstUser == "" && len(ev.Blocks) > 0 {
					firstUser = ev.Blocks[0].Text
				}
			} else if ev.Role == "assistant" {
				asstMsgs++
			}
		case domain.EventToolCall:
			toolSet[ev.ToolName] = struct{}{}
		}
	}
	tools := make([]string, 0, len(toolSet))
	for t := range toolSet {
		tools = append(tools, t)
	}
	sort.Strings(tools)

	if compactSummary != "" {
		// Claude compression summaries usually contain stable numbered sections
		// (empirically 42/43 samples used "Key Technical Concepts" and "Pending Tasks").
		// Parse the latest occurrence because summaries are cumulative.
		facts, tasks := extractSummarySections(compactSummary)
		structured := len(facts) > 0 || len(tasks) > 0
		if len(facts) == 0 {
			facts = tools // unstructured summary (different format) — maintain existing fallback
		}
		// Source marker ("native memory: …") is not added to KeyFacts — native memory is loaded by agent at session start (review P2).
		return domain.MemoryDigest{
			Summary:  truncate(compactSummary, 16000),
			KeyFacts: facts,
			// When structure extraction succeeds, this list becomes the merge authority (parent completion not inherited).
			OpenTasks:          tasks,
			TasksAuthoritative: structured,
			Provider:           prov,
		}, nil
	}
	if native != nil {
		// Native-first ingestion (no compression summary).
		return domain.MemoryDigest{
			Summary:  native.Text,
			KeyFacts: []string{"ingested from " + native.Source},
			Provider: prov,
		}, nil
	}
	// Codex compaction bodies can be provider-encrypted while payload.message is
	// empty. The raw CIR is still losslessly available, so preserve a bounded
	// extractive digest of meaningful conversation instead of reducing memory to
	// counters and tool names. This is intentionally deterministic: content hashes
	// must not depend on an external model or provider-owned decryption.
	summary := extractiveConversationDigest(cir.Events)
	if summary == "" {
		// Empty/non-conversational documents retain the old diagnostic fallback.
		// Storage artifacts are the English data layer, independent of UI language.
		summary = fmt.Sprintf("session summary: branch %q · %d user / %d assistant messages · tools %v · first request: %q",
			cir.Envelope.GitBranch, userMsgs, asstMsgs, tools, truncate(firstUser, 120))
	}
	return domain.MemoryDigest{
		Summary:  summary,
		Provider: prov,
	}, nil
}

const (
	extractiveDigestMaxRunes = 16000
	extractiveItemMaxRunes   = 2000
	extractiveItemsPerRole   = 6
)

// extractiveConversationDigest preserves recent user intent and assistant
// outcomes when the provider's own compaction summary is unavailable. Tool
// calls/results and compact-summary events are excluded; the latter already
// have a higher-priority path above and synthetic branch seeds can be very
// large. Selection is recent-first but rendered chronologically.
func extractiveConversationDigest(events []domain.Event) string {
	var users, assistants []string
	for _, ev := range events {
		if ev.Kind != domain.EventMessage || ev.CompactSummary {
			continue
		}
		text := messageText(ev.Blocks)
		if text == "" || isSyntheticSeedText(text) {
			continue
		}
		switch ev.Role {
		case "user":
			users = appendRecentDistinct(users, text, extractiveItemsPerRole)
		case "assistant":
			assistants = appendRecentDistinct(assistants, text, extractiveItemsPerRole)
		}
	}
	if len(users) == 0 && len(assistants) == 0 {
		return ""
	}
	return truncate(domain.RenderExtractiveFallbackSummary(users, assistants), extractiveDigestMaxRunes-1)
}

func messageText(blocks []domain.ContentBlock) string {
	var parts []string
	for _, block := range blocks {
		if block.Type != "text" || strings.TrimSpace(block.Text) == "" {
			continue
		}
		parts = append(parts, strings.TrimSpace(block.Text))
	}
	return strings.Join(parts, "\n")
}

func isSyntheticSeedText(text string) bool {
	t := strings.TrimSpace(text)
	// Resume seeds start life as CompactSummary events, but materializing into a
	// provider rollout and re-capturing them yields plain user messages (measured
	// in real Codex digests) — match the boilerplate by prefix as well. Harness
	// environment_context blocks are machine-generated state, not user intent.
	return strings.HasPrefix(t, "[cxt seed] Branch-switch context:") ||
		strings.HasPrefix(t, "[cxt] This session was resumed from a branch context seed.") ||
		strings.HasPrefix(t, "<environment_context>")
}

// containsSyntheticSeedText rejects an agent-written compaction summary that
// has recursively swallowed a cxt seed. Treating it as a new authoritative
// generation creates summary-of-summary amplification on every resume. The raw
// recent conversation remains available to the deterministic extractive
// fallback, while older project memory stays in provenance fragments.
func containsSyntheticSeedText(text string) bool {
	if isSyntheticSeedText(text) {
		return true
	}
	return strings.Contains(text, "[cxt] This session was resumed from a branch context seed.") ||
		strings.Contains(text, "[cxt seed] Branch-switch context:")
}

func appendRecentDistinct(items []string, text string, limit int) []string {
	text = strings.Join(strings.Fields(text), " ")
	text = truncate(text, extractiveItemMaxRunes)
	if text == "" || (len(items) > 0 && items[len(items)-1] == text) {
		return items
	}
	items = append(items, text)
	if len(items) > limit {
		items = items[len(items)-limit:]
	}
	return items
}

// extractSummarySections extracts the top-level bullets from the KeyFacts ("Key Technical Concepts") and OpenTasks ("Pending Tasks") sections of a compression summary. It accumulates summaries, so it writes the last occurrence of each section. For unstructured text, it returns an empty result (fallback for the caller). Deterministic.
func extractSummarySections(summary string) (facts, tasks []string) {
	lines := strings.Split(summary, "\n")
	facts = sectionBullets(lines, lastSectionStart(lines, "key technical concepts"))
	tasks = sectionBullets(lines, lastSectionStart(lines, "pending tasks"))
	return facts, tasks
}

// numberedHeader determines if a section header is in the "N. Title:" format (indented by 3 spaces or less — section body bullets are indented deeper).
func numberedHeader(line string) bool {
	t := strings.TrimLeft(line, " ")
	if len(line)-len(t) > 3 || len(t) < 3 || t[0] < '1' || t[0] > '9' {
		return false
	}
	i := 1
	for i < len(t) && t[i] >= '0' && t[i] <= '9' {
		i++
	}
	return i+1 < len(t) && t[i] == '.' && t[i+1] == ' '
}

// lastSectionStart returns the index of the last line containing a numbered header with the titleLower (or -1 if none).
func lastSectionStart(lines []string, titleLower string) int {
	start := -1
	for i, ln := range lines {
		if numberedHeader(ln) && strings.Contains(strings.ToLower(ln), titleLower) {
			start = i
		}
	}
	return start
}

// sectionBullets collects the top-level bullets from the line after the section start to the line before the next numbered header. It takes only the first line of each bullet (ignoring consecutive line breaks for detailed descriptions) and strips markdown emphasis.
func sectionBullets(lines []string, start int) []string {
	if start < 0 {
		return nil
	}
	const (
		maxIndent = 6   // Only top-level bullets (empirically 3 spaces indented) — nested details are excluded
		maxItems  = 10  // Seed header length limit
		maxLen    = 300 // max length per item (bullet first line)
	)
	var out []string
	for i := start + 1; i < len(lines); i++ {
		if numberedHeader(lines[i]) {
			break
		}
		t := strings.TrimLeft(lines[i], " \t")
		if len(lines[i])-len(t) > maxIndent || !strings.HasPrefix(t, "- ") {
			continue
		}
		item := strings.TrimSpace(strings.ReplaceAll(t[2:], "**", ""))
		if item == "" {
			continue
		}
		out = append(out, truncate(item, maxLen))
		if len(out) >= maxItems {
			break
		}
	}
	return out
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// Ensure RuleDistiller implements outbound.MemoryDistiller.
var _ outbound.MemoryDistiller = (*RuleDistiller)(nil)
