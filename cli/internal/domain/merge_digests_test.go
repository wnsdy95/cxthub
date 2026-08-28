package domain

import (
	"reflect"
	"strings"
	"testing"
)

// TestMergeDigestsOpenTasksAuthority fixes the OpenTasks merge authority rules:
// If fresh is a struct extraction (TasksAuthoritative), it does not inherit the ancestor list — to prevent regressions in completed tasks being permanently accumulated in the lineage (review P1). Non-struct fresh behaves as before, prioritizing the union of prior.
func TestMergeDigestsOpenTasksAuthority(t *testing.T) {
	prior := MemoryDigest{OpenTasks: []string{"done task from last week", "still open"}}

	fresh := MemoryDigest{OpenTasks: []string{"still open", "new task"}, TasksAuthoritative: true}
	got := MergeDigests(prior, fresh)
	if !reflect.DeepEqual(got.OpenTasks, []string{"still open", "new task"}) {
		t.Fatalf("Authority fresh but ancestor list inherited: %v", got.OpenTasks)
	}

	// Empty list for authority fresh is respected (all tasks completed states).
	got = MergeDigests(prior, MemoryDigest{TasksAuthoritative: true})
	if len(got.OpenTasks) != 0 {
		t.Fatalf("Empty list for authority fresh but ancestor remains: %v", got.OpenTasks)
	}

	// Non-struct fresh: maintains previous behavior (prior union and dedup).
	got = MergeDigests(prior, MemoryDigest{OpenTasks: []string{"still open", "another"}})
	want := []string{"done task from last week", "still open", "another"}
	if !reflect.DeepEqual(got.OpenTasks, want) {
		t.Fatalf("Non-struct merge = %v, want %v", got.OpenTasks, want)
	}
}

func TestRenderExtractiveFallbackSummaryPreservesWireFormat(t *testing.T) {
	want := "Conversation digest (extractive fallback; provider compaction summary unavailable):\n\n\n" +
		"Recent user intent:\n- Keep the existing hash contract.\n\n\n" +
		"Recent assistant outcomes:\n- Verified the canonical whitespace."
	got := RenderExtractiveFallbackSummary(
		[]string{"Keep the existing hash contract."},
		[]string{"Verified the canonical whitespace."},
	)
	if got != want {
		t.Fatalf("extractive fallback wire format changed:\n got %q\nwant %q", got, want)
	}
}

func TestMergeDigestsDeduplicatesExtractiveFallbackItems(t *testing.T) {
	prior := MemoryDigest{
		SnapshotID: ContentHash("sha256:" + strings.Repeat("a", 64)),
		Summary: extractiveFallbackSummary(
			[]string{"Inspect pull context boundaries.", "Fix every confirmed defect."},
			[]string{"Measured the restart seed."},
		),
	}
	fresh := MemoryDigest{
		SnapshotID: ContentHash("sha256:" + strings.Repeat("b", 64)),
		Summary: extractiveFallbackSummary(
			[]string{"Inspect pull context boundaries.", "Fix every confirmed defect."},
			[]string{"Measured the restart seed.", "Scoped pull briefings by terminal."},
		),
	}

	got := MergeDigests(prior, fresh)
	if len(got.Fragments) != 2 {
		t.Fatalf("provenance fragments = %d, want 2", len(got.Fragments))
	}
	for _, item := range []string{
		"Conversation digest (extractive fallback; provider compaction summary unavailable):",
		"Inspect pull context boundaries.",
		"Fix every confirmed defect.",
		"Measured the restart seed.",
	} {
		if count := strings.Count(got.Summary, item); count != 1 {
			t.Fatalf("rendered summary count(%q) = %d, want 1:\n%s", item, count, got.Summary)
		}
	}
	if !strings.Contains(got.Summary, "Scoped pull briefings by terminal.") {
		t.Fatalf("new unique outcome was lost:\n%s", got.Summary)
	}
}

func TestMergeDigestsDeduplicatesBaselineAndKeepsConversationDeltas(t *testing.T) {
	leftID := ContentHash("sha256:" + strings.Repeat("7", 64))
	rightID := ContentHash("sha256:" + strings.Repeat("8", 64))
	const baseline = "PROVIDER COMPACT BASELINE REMAINS BYTE STABLE"
	left := MemoryDigest{
		SnapshotID: leftID,
		Summary: AppendExtractiveConversationDelta(baseline, RenderExtractiveFallbackSummary(
			[]string{"Decision from the left lineage."},
			[]string{"Result from the left lineage."},
		)),
	}
	right := MemoryDigest{
		SnapshotID: rightID,
		Summary: AppendExtractiveConversationDelta(baseline, RenderExtractiveFallbackSummary(
			[]string{"Decision from the right lineage."},
			[]string{"Result from the right lineage."},
		)),
	}

	got := MergeDigests(left, right)
	if count := strings.Count(got.Summary, baseline); count != 1 {
		t.Fatalf("merged baseline count=%d, want 1:\n%s", count, got.Summary)
	}
	if count := strings.Count(got.Summary, extractiveFallbackHeader); count != 1 {
		t.Fatalf("merged extractive header count=%d, want 1:\n%s", count, got.Summary)
	}
	for _, want := range []string{
		"Decision from the left lineage.", "Result from the left lineage.",
		"Decision from the right lineage.", "Result from the right lineage.",
	} {
		if !strings.Contains(got.Summary, want) {
			t.Fatalf("merged memory lost %q:\n%s", want, got.Summary)
		}
	}

	prompt := WithoutConversationDeltaFromSource(got, rightID)
	for _, kept := range []string{baseline, "Decision from the left lineage.", "Result from the left lineage."} {
		if !strings.Contains(prompt.Summary, kept) {
			t.Fatalf("source-filtered prompt lost %q:\n%s", kept, prompt.Summary)
		}
	}
	for _, removed := range []string{"Decision from the right lineage.", "Result from the right lineage."} {
		if strings.Contains(prompt.Summary, removed) {
			t.Fatalf("source-filtered prompt retained %q:\n%s", removed, prompt.Summary)
		}
		if !strings.Contains(got.Summary, removed) {
			t.Fatalf("source filtering mutated the immutable input projection: %q", removed)
		}
	}
}

func TestWithoutConversationDeltaLeavesUnstructuredProviderTextUntouched(t *testing.T) {
	id := ContentHash("sha256:" + strings.Repeat("9", 64))
	providerFallback := RenderExtractiveFallbackSummary(
		[]string{"This sentence was authored by the provider."},
		[]string{"It must remain part of the provider baseline."},
	)
	for _, summary := range []string{
		"Provider prose mentioning " + extractiveFallbackHeader + " without the canonical sections",
		"Provider-authored baseline\n\n" + providerFallback,
	} {
		digest := MemoryDigest{SnapshotID: id, Summary: summary}
		if got := WithoutConversationDeltaFromSource(digest, id); got.Summary != summary {
			t.Fatalf("unstructured provider text changed: got %q want %q", got.Summary, summary)
		}
	}
}

func TestWithoutExactSummaryBaselinePreservesDeltasAndUnrelatedFragments(t *testing.T) {
	leftID := ContentHash("sha256:" + strings.Repeat("1", 64))
	rightID := ContentHash("sha256:" + strings.Repeat("2", 64))
	otherID := ContentHash("sha256:" + strings.Repeat("3", 64))
	const baseline = "EXACT AUTO-LOADED PROVIDER BASELINE"
	digest := MemoryDigest{Fragments: []MemoryFragment{
		{
			SourceSnapshot: leftID,
			Summary: AppendExtractiveConversationDelta(baseline, RenderExtractiveFallbackSummary(
				[]string{"Left lineage decision remains."}, nil)),
		},
		{
			SourceSnapshot: rightID,
			Summary: AppendExtractiveConversationDelta(baseline, RenderExtractiveFallbackSummary(
				nil, []string{"Right lineage result remains."})),
		},
		{SourceSnapshot: otherID, Summary: "UNRELATED PROVIDER BASELINE"},
	}}
	digest = MergeDigests(MemoryDigest{}, digest)

	got := WithoutExactSummaryBaseline(digest, baseline)
	if strings.Contains(got.Summary, baseline) {
		t.Fatalf("exact auto-loaded baseline remains:\n%s", got.Summary)
	}
	for _, want := range []string{
		"Left lineage decision remains.", "Right lineage result remains.", "UNRELATED PROVIDER BASELINE",
	} {
		if !strings.Contains(got.Summary, want) {
			t.Fatalf("baseline projection lost %q:\n%s", want, got.Summary)
		}
	}
	if !strings.Contains(digest.Summary, baseline) {
		t.Fatal("prompt projection mutated the stored digest")
	}

	legacy := MemoryDigest{SnapshotID: leftID, Summary: baseline}
	if stripped := WithoutExactSummaryBaseline(legacy, baseline); stripped.Summary != "" {
		t.Fatalf("legacy exact baseline = %q, want empty", stripped.Summary)
	}
	containing := MemoryDigest{SnapshotID: leftID, Summary: "prefix " + baseline}
	if kept := WithoutExactSummaryBaseline(containing, baseline); kept.Summary != containing.Summary {
		t.Fatalf("non-exact provider text changed: got %q want %q", kept.Summary, containing.Summary)
	}
}

func TestPromptStructuredProjectionKeepsOnlyAttestedPromptState(t *testing.T) {
	ancestorID := ContentHash("sha256:" + strings.Repeat("4", 64))
	currentID := ContentHash("sha256:" + strings.Repeat("5", 64))
	latestID := ContentHash("sha256:" + strings.Repeat("6", 64))
	digest := MemoryDigest{
		SnapshotID: latestID,
		Provider:   ProviderCodex,
		Fragments: []MemoryFragment{
			{
				SourceSnapshot: ancestorID,
				Summary:        "PROVIDER BASELINE",
				KeyFacts: []string{
					"Overlay graft parents remain immutable.",
					"apply_patch",
					"native memory: claude:MEMORY.md",
				},
				OpenTasks: []string{
					"Completed task from an extractive fallback.",
					"<environment_context>runtime state</environment_context>",
				},
			},
			{
				SourceSnapshot: currentID,
				Summary:        "CURRENT PROVIDER STATE",
				KeyFacts:       []string{"The prompt projection must preserve valid project facts."},
				OpenTasks: []string{
					"Fix the remaining authoritative task.",
					"Overlay graft parents remain immutable.",
					"continue",
					"[cxt] This session was resumed from a branch context seed.",
				},
				TasksAuthoritative: true,
			},
			{
				SourceSnapshot: latestID,
				KeyFacts:       []string{"A later non-authoritative fact is still useful project knowledge."},
				OpenTasks:      []string{"A later fallback must not revive stale tasks."},
			},
		},
	}
	digest = MergeDigests(MemoryDigest{}, digest)
	original := cloneMemoryDigestForTest(digest)

	got := PromptStructuredProjection(digest)
	if !reflect.DeepEqual(got.KeyFacts, []string{
		"Overlay graft parents remain immutable.",
		"The prompt projection must preserve valid project facts.",
		"A later non-authoritative fact is still useful project knowledge.",
	}) {
		t.Fatalf("prompt facts = %#v", got.KeyFacts)
	}
	if !reflect.DeepEqual(got.OpenTasks, []string{"Fix the remaining authoritative task."}) {
		t.Fatalf("prompt tasks = %#v", got.OpenTasks)
	}
	if len(got.Fragments) != 3 || len(got.Fragments[0].OpenTasks) != 0 ||
		!reflect.DeepEqual(got.Fragments[1].OpenTasks, got.OpenTasks) || len(got.Fragments[2].OpenTasks) != 0 {
		t.Fatalf("projected fragment tasks = %#v", got.Fragments)
	}
	for _, want := range []string{"PROVIDER BASELINE", "CURRENT PROVIDER STATE"} {
		if !strings.Contains(got.Summary, want) {
			t.Fatalf("prompt projection lost summary %q:\n%s", want, got.Summary)
		}
	}
	if !reflect.DeepEqual(digest, original) {
		t.Fatal("prompt projection mutated the stored digest")
	}
}

func TestPromptStructuredProjectionPreservesAuthoritativeEmptyTaskTombstone(t *testing.T) {
	ids := []ContentHash{
		ContentHash("sha256:" + strings.Repeat("7", 64)),
		ContentHash("sha256:" + strings.Repeat("8", 64)),
		ContentHash("sha256:" + strings.Repeat("9", 64)),
	}
	digest := MemoryDigest{Fragments: []MemoryFragment{
		{SourceSnapshot: ids[0], OpenTasks: []string{"Previously active task."}, TasksAuthoritative: true},
		{SourceSnapshot: ids[1], OpenTasks: []string{"Unattested fallback task."}},
		{SourceSnapshot: ids[2], TasksAuthoritative: true},
	}}
	digest = MergeDigests(MemoryDigest{}, digest)

	got := PromptStructuredProjection(digest)
	if len(got.OpenTasks) != 0 {
		t.Fatalf("authoritative empty task state revived ancestors: %v", got.OpenTasks)
	}
	if len(got.Fragments) != 3 || !got.Fragments[2].TasksAuthoritative {
		t.Fatalf("authoritative tombstone lost: %#v", got.Fragments)
	}

	legacy := MemoryDigest{OpenTasks: []string{"Ambiguous legacy task must stay archived."}}
	if projected := PromptStructuredProjection(legacy); len(projected.OpenTasks) != 0 {
		t.Fatalf("ambiguous legacy tasks entered prompt: %v", projected.OpenTasks)
	}
	authoritative := MemoryDigest{
		OpenTasks:          []string{"Current attested task remains."},
		TasksAuthoritative: true,
	}
	if projected := PromptStructuredProjection(authoritative); !reflect.DeepEqual(projected.OpenTasks, authoritative.OpenTasks) {
		t.Fatalf("attested legacy task projection = %v", projected.OpenTasks)
	}
}

func cloneMemoryDigestForTest(d MemoryDigest) MemoryDigest {
	out := d
	out.KeyFacts = append([]string(nil), d.KeyFacts...)
	out.OpenTasks = append([]string(nil), d.OpenTasks...)
	out.Fragments = append([]MemoryFragment(nil), d.Fragments...)
	for i := range out.Fragments {
		out.Fragments[i].KeyFacts = append([]string(nil), d.Fragments[i].KeyFacts...)
		out.Fragments[i].OpenTasks = append([]string(nil), d.Fragments[i].OpenTasks...)
	}
	return out
}

func TestWithoutAutoLoadedSummaryPrefixKeepsNativeTail(t *testing.T) {
	id := ContentHash("sha256:" + strings.Repeat("4", 64))
	const (
		loadedPrefix = "AUTO-LOADED LINE ONE\nAUTO-LOADED LINE TWO"
		nativeTail   = "NATIVE TAIL NOT LOADED BY PROVIDER"
	)
	full := loadedPrefix + "\n" + nativeTail
	digest := MemoryDigest{
		SnapshotID: id,
		Summary: AppendExtractiveConversationDelta(full, RenderExtractiveFallbackSummary(
			[]string{"Conversation delta remains."}, nil)),
	}
	digest = MergeDigests(MemoryDigest{}, digest)

	got := WithoutAutoLoadedSummaryPrefix(digest, full, loadedPrefix)
	if strings.Contains(got.Summary, loadedPrefix) {
		t.Fatalf("safe auto-loaded prefix remains:\n%s", got.Summary)
	}
	for _, want := range []string{nativeTail, "Conversation delta remains."} {
		if !strings.Contains(got.Summary, want) {
			t.Fatalf("partial native projection lost %q:\n%s", want, got.Summary)
		}
	}
	if !strings.Contains(digest.Summary, loadedPrefix) {
		t.Fatal("partial prompt projection mutated stored digest")
	}

	if unchanged := WithoutAutoLoadedSummaryPrefix(digest, full, "not an exact prefix"); unchanged.Summary != digest.Summary {
		t.Fatalf("non-prefix projection changed digest: got %q want %q", unchanged.Summary, digest.Summary)
	}
}

func TestWithoutExactReplayedSummaryDropsMatchingBaselinesAndKeepsOtherLineages(t *testing.T) {
	currentID := ContentHash("sha256:" + strings.Repeat("a", 64))
	ancestorID := ContentHash("sha256:" + strings.Repeat("b", 64))
	matchingAncestorID := ContentHash("sha256:" + strings.Repeat("e", 64))
	const replayed = "PROVIDER COMPACTION ALREADY REPLAYED"
	digest := MergeDigests(
		MergeDigests(
			MemoryDigest{
				SnapshotID: ancestorID,
				Summary:    "ANCESTOR MEMORY MUST REMAIN",
				KeyFacts:   []string{"Ancestor fact remains."},
				OpenTasks:  []string{"Superseded ancestor task must not revive."},
			},
			MemoryDigest{
				SnapshotID: matchingAncestorID,
				Summary: AppendExtractiveConversationDelta(replayed,
					RenderExtractiveFallbackSummary([]string{"teammate request remains"}, []string{"teammate result remains"})),
			},
		),
		MemoryDigest{
			SnapshotID: currentID,
			Summary: AppendExtractiveConversationDelta(replayed,
				RenderExtractiveFallbackSummary([]string{"current request"}, []string{"current result"})),
			KeyFacts:           []string{"Fact already inside replayed summary."},
			OpenTasks:          []string{"Task already inside replayed summary."},
			TasksAuthoritative: true,
		},
	)

	got := WithoutExactReplayedSummary(digest, replayed)
	got = WithoutExactReplayedConversationItems(got, []string{"current request"}, []string{"current result"})
	for _, removed := range []string{
		replayed, "current request", "current result",
		"Fact already inside replayed summary.", "Task already inside replayed summary.",
	} {
		if strings.Contains(got.Summary, removed) || containsString(got.KeyFacts, removed) || containsString(got.OpenTasks, removed) {
			t.Fatalf("replayed current contribution retained %q: %+v", removed, got)
		}
	}
	for _, kept := range []string{
		"ANCESTOR MEMORY MUST REMAIN", "Ancestor fact remains.",
		"teammate request remains", "teammate result remains",
	} {
		if !strings.Contains(got.Summary, kept) && !containsString(got.KeyFacts, kept) {
			t.Fatalf("replayed projection lost %q: %+v", kept, got)
		}
	}
	if !strings.Contains(digest.Summary, replayed) || !containsString(digest.KeyFacts, "Fact already inside replayed summary.") {
		t.Fatalf("projection mutated immutable input: %+v", digest)
	}
	if len(got.OpenTasks) != 0 || !got.TasksAuthoritative {
		t.Fatalf("replayed authoritative task tombstone was lost: %+v", got)
	}
}

func TestWithoutExactReplayedConversationItemsKeepsOnlyNonReplayLineageDelta(t *testing.T) {
	ancestorID := ContentHash("sha256:" + strings.Repeat("c", 64))
	siblingID := ContentHash("sha256:" + strings.Repeat("d", 64))
	longRaw := strings.Repeat("L", extractiveConversationItemMaxRunes+100)
	digest := MergeDigests(
		MemoryDigest{
			SnapshotID: ancestorID,
			Summary: AppendExtractiveConversationDelta("PORTABLE BASELINE",
				RenderExtractiveFallbackSummary(
					[]string{"raw request", NormalizeExtractiveConversationItem(longRaw)},
					[]string{"raw result"},
				)),
		},
		MemoryDigest{
			SnapshotID: siblingID,
			Summary: AppendExtractiveConversationDelta("PORTABLE BASELINE",
				RenderExtractiveFallbackSummary([]string{"teammate request"}, []string{"teammate result"})),
		},
	)

	got := WithoutExactReplayedConversationItems(digest, []string{" raw   request ", longRaw}, []string{"raw result"})
	for _, removed := range []string{"raw request", "raw result", strings.Repeat("L", 64)} {
		if strings.Contains(got.Summary, removed) {
			t.Fatalf("replayed conversation item retained %q:\n%s", removed, got.Summary)
		}
	}
	for _, kept := range []string{"PORTABLE BASELINE", "teammate request", "teammate result"} {
		if !strings.Contains(got.Summary, kept) {
			t.Fatalf("portable projection lost %q:\n%s", kept, got.Summary)
		}
	}
	if !strings.Contains(digest.Summary, "raw request") {
		t.Fatalf("projection mutated immutable input: %+v", digest)
	}
}

func TestMergeDigestsPreservesUniqueSiblingFallbackItems(t *testing.T) {
	left := MemoryDigest{
		SnapshotID: ContentHash("sha256:" + strings.Repeat("c", 64)),
		Summary: extractiveFallbackSummary(
			[]string{"Review the left branch."},
			[]string{"Verified the left branch migration."},
		),
	}
	right := MemoryDigest{
		SnapshotID: ContentHash("sha256:" + strings.Repeat("d", 64)),
		Summary: extractiveFallbackSummary(
			[]string{"Review the right branch."},
			[]string{"Verified the right branch migration."},
		),
	}

	got := MergeDigests(left, right)
	for _, item := range []string{
		"Review the left branch.",
		"Review the right branch.",
		"Verified the left branch migration.",
		"Verified the right branch migration.",
	} {
		if !strings.Contains(got.Summary, item) {
			t.Fatalf("sibling item %q was lost:\n%s", item, got.Summary)
		}
	}
	if strings.Index(got.Summary, "Review the left branch.") > strings.Index(got.Summary, "Review the right branch.") ||
		strings.Index(got.Summary, "Verified the left branch migration.") > strings.Index(got.Summary, "Verified the right branch migration.") {
		t.Fatalf("sibling chronology changed:\n%s", got.Summary)
	}
}

func TestMergeDigestsKeepsUnstructuredSummariesOrdered(t *testing.T) {
	leftFallback := MemoryFragment{
		SourceSnapshot: ContentHash("sha256:" + strings.Repeat("e", 64)),
		Summary:        extractiveFallbackSummary([]string{"Shared request."}, []string{"Left outcome."}),
	}
	leftAgent := MemoryFragment{
		SourceSnapshot: ContentHash("sha256:" + strings.Repeat("f", 64)),
		Summary:        "Agent-written summary A\nwith deliberate formatting.",
	}
	rightFallback := MemoryFragment{
		SourceSnapshot: ContentHash("sha256:" + strings.Repeat("1", 64)),
		Summary:        extractiveFallbackSummary([]string{"Shared request."}, []string{"Right outcome."}),
	}
	rightAgent := MemoryFragment{
		SourceSnapshot: ContentHash("sha256:" + strings.Repeat("2", 64)),
		Summary:        "Agent-written summary B\nwith different formatting.",
	}

	got := MergeDigests(
		MemoryDigest{Fragments: []MemoryFragment{leftFallback, leftAgent}},
		MemoryDigest{Fragments: []MemoryFragment{rightFallback, rightAgent}},
	)
	if strings.Count(got.Summary, "Shared request.") != 1 ||
		!strings.Contains(got.Summary, "Left outcome.") || !strings.Contains(got.Summary, "Right outcome.") {
		t.Fatalf("fallback projection was not normalized:\n%s", got.Summary)
	}
	leftAt := strings.Index(got.Summary, leftAgent.Summary)
	fallbackAt := strings.Index(got.Summary, "Conversation digest (extractive fallback; provider compaction summary unavailable):")
	rightAt := strings.Index(got.Summary, rightAgent.Summary)
	if leftAt < 0 || fallbackAt < 0 || rightAt < 0 || !(leftAt < fallbackAt && fallbackAt < rightAt) {
		t.Fatalf("unstructured summaries changed or reordered:\n%s", got.Summary)
	}
	if len(got.Fragments) != 4 {
		t.Fatalf("mixed provenance fragments = %d, want 4", len(got.Fragments))
	}
}

func TestMergeDigestsDoesNotParseFallbackLookalike(t *testing.T) {
	lookalike := extractiveFallbackSummary([]string{"Valid-looking item."}, nil) +
		"\nAgent commentary outside the deterministic sections."
	valid := extractiveFallbackSummary([]string{"Actual deterministic item."}, nil)
	got := MergeDigests(
		MemoryDigest{SnapshotID: ContentHash("sha256:" + strings.Repeat("3", 64)), Summary: lookalike},
		MemoryDigest{SnapshotID: ContentHash("sha256:" + strings.Repeat("4", 64)), Summary: valid},
	)
	if !strings.Contains(got.Summary, lookalike) {
		t.Fatalf("fallback lookalike was rewritten:\n%s", got.Summary)
	}
	if !strings.Contains(got.Summary, valid) {
		t.Fatalf("valid fallback was lost beside lookalike:\n%s", got.Summary)
	}
}

func extractiveFallbackSummary(users, assistants []string) string {
	var b strings.Builder
	b.WriteString("Conversation digest (extractive fallback; provider compaction summary unavailable):\n")
	if len(users) > 0 {
		b.WriteString("\n\nRecent user intent:\n")
		for _, item := range users {
			b.WriteString("- ")
			b.WriteString(item)
			b.WriteByte('\n')
		}
	}
	if len(assistants) > 0 {
		b.WriteString("\n\nRecent assistant outcomes:\n")
		for _, item := range assistants {
			b.WriteString("- ")
			b.WriteString(item)
			b.WriteByte('\n')
		}
	}
	return strings.TrimSpace(b.String())
}
