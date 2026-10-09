package app

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/memory"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

// Seed an already captured document with attachment A without recording this
// attempt's observation. All bytes come from frozenFixture's synthetic spool.
func seedFrozenMemoryAttachment(t *testing.T, f stagingFixture, p domain.CaptureAttempt) (domain.ContentHash, domain.ContentHash) {
	t.Helper()
	ctx := context.Background()
	in := *p.Outcomes[0].Input
	frozen := f.svc.save.capture.(outbound.FrozenSessionCapture)
	env, ref, _, _, err := frozen.ProjectFrozen(ctx, f.root, in, f.svc.save.captures[in.Provider], f.svc.save.codecs[in.Provider])
	if err != nil {
		t.Fatal(err)
	}
	a, err := f.store.PutMemory(ctx, domain.MemoryDigest{SnapshotID: ref.Hash, Provider: in.Provider, Summary: "attachment A"})
	if err != nil {
		t.Fatal(err)
	}
	err = f.store.PutSnapshot(ctx, domain.Snapshot{ID: ref.Hash, DocHash: ref.Hash, DocIdentity: ref.Identity,
		RepoID: f.git.repo.ID, Branch: p.Proof.Branch, Parents: []domain.ContentHash{p.Initial},
		Provider: in.Provider, SessionID: env.SessionOriginID, MemoryHash: a,
		Message: domain.HookMessagePrefix + " synthetic capture", CreatedAt: p.Proof.CreatedAt})
	if err != nil {
		t.Fatal(err)
	}
	return ref.Hash, a
}

// Round-trip the plan with the private receipt before allowing SaveFrozen to
// attach it, just as a restarted worker receives the journaled predecessor/result.
func prepareFrozenMemoryReceipt(t *testing.T, f stagingFixture, p domain.CaptureAttempt) domain.CaptureAttempt {
	t.Helper()
	plan, err := f.svc.save.PrepareFrozenMemory(context.Background(), f.root, p, 0)
	if err != nil || plan == nil {
		t.Fatalf("prepare frozen memory: plan=%+v err=%v", plan, err)
	}
	p.Outcomes[0].MemoryPlan = plan
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	var restored domain.CaptureAttempt
	if err := json.Unmarshal(raw, &restored); err != nil {
		t.Fatal(err)
	}
	if err := restored.Validate(); err != nil {
		t.Fatal("invalid persisted memory plan", err)
	}
	return restored
}

type frozenMemorySelection struct {
	position domain.WorkingPosition
	ref      domain.Ref
}

func readFrozenMemorySelection(t *testing.T, f stagingFixture) frozenMemorySelection {
	t.Helper()
	ctx := context.Background()
	position, err := f.store.GetWorkingPosition(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := f.store.GetRef(ctx, f.git.repo.ID, domain.RefBranch, position.Branch)
	if err != nil {
		t.Fatal(err)
	}
	return frozenMemorySelection{position, ref}
}

func requireFrozenMemorySelection(t *testing.T, f stagingFixture, before frozenMemorySelection) {
	t.Helper()
	if after := readFrozenMemorySelection(t, f); !reflect.DeepEqual(before, after) {
		t.Fatalf("frozen memory changed active cursor/ref: before=%+v after=%+v", before, after)
	}
}

func TestFrozenMemoryPreparationLeavesSelectionAndAttachmentsUnchanged(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(map[bool]string{false: "new-document", true: "existing-attachment"}[existing], func(t *testing.T) {
			f, p := frozenFixture(t)
			ctx := context.Background()
			f.svc.save.WithFrozenMemory(nil, memory.NewRuleDistiller())
			p.Outcomes[0].Input.NativeMemory = &domain.NativeMemory{Provider: domain.ProviderClaude, Text: "FROZEN NATIVE MEMORY"}
			var target, a domain.ContentHash
			if existing {
				target, a = seedFrozenMemoryAttachment(t, f, p)
			}
			before := readFrozenMemorySelection(t, f)
			snapshots, err := f.store.ListSnapshots(ctx, f.git.repo.ID, "")
			if err != nil {
				t.Fatal(err)
			}
			history, err := f.store.ListHistoryEvents(ctx, f.git.repo.ID)
			if err != nil {
				t.Fatal(err)
			}

			p = prepareFrozenMemoryReceipt(t, f, p)
			plan := p.Outcomes[0].MemoryPlan
			if !existing {
				original, err := f.store.GetMemory(ctx, p.Proof.MemoryHash)
				if err != nil {
					t.Fatal(err)
				}
				inherited := domain.MergeDigests(original, domain.MemoryDigest{SnapshotID: plan.Snapshot, Provider: p.Outcomes[0].Provider})
				inherited.PreviousMemoryHash = ""
				a, err = domain.MemoryDigestHash(inherited)
				if err != nil {
					t.Fatal(err)
				}
			}
			if plan.ExpectedMemory != a || plan.Memory == a || (existing && plan.Snapshot != target) {
				t.Fatalf("plan lost the observed attachment: %+v; original=%s", plan, a)
			}
			prepared, err := f.store.GetMemory(ctx, plan.Memory)
			if err != nil {
				t.Fatal("prepared immutable memory is not durable", err)
			}
			if prepared.SnapshotID != plan.Snapshot || prepared.PreviousMemoryHash != a {
				t.Fatalf("prepared memory has wrong causal owner/parent: %+v", prepared)
			}
			for _, text := range []string{"ORIGINAL MEMORY", "FROZEN NATIVE MEMORY", "only the frozen message"} {
				if !strings.Contains(prepared.Summary, text) {
					t.Errorf("prepared memory lost frozen contribution %q", text)
				}
			}
			afterSnapshots, err := f.store.ListSnapshots(ctx, f.git.repo.ID, "")
			if err != nil || !reflect.DeepEqual(snapshots, afterSnapshots) {
				t.Fatal("preparation created/changed a snapshot or its attachment", err)
			}
			afterHistory, err := f.store.ListHistoryEvents(ctx, f.git.repo.ID)
			if err != nil || !reflect.DeepEqual(history, afterHistory) {
				t.Fatal("preparation published a memory observation", err)
			}
			requireFrozenMemorySelection(t, f, before)

			out, err := f.svc.save.SaveFrozen(ctx, f.root, p, 0)
			if err != nil {
				t.Fatal("save prepared memory", err)
			}
			if out.SnapshotID != plan.Snapshot || out.MemoryHash != plan.Memory || out.MemorySource != plan.Snapshot {
				t.Fatalf("save did not use the persisted plan: %+v; plan=%+v", out, plan)
			}
			snap, err := f.store.GetSnapshot(ctx, plan.Snapshot)
			if err != nil || snap.MemoryHash != plan.Memory {
				t.Fatal("save did not attach the prepared memory", err)
			}
			requireFrozenMemorySelection(t, f, before)
		})
	}
}

func TestFrozenMemoryLivePendingCreationMatchesPreparedInitialMemory(t *testing.T) {
	for _, competing := range []bool{false, true} {
		t.Run(map[bool]string{false: "same-imported-A", true: "unrelated-C"}[competing], func(t *testing.T) {
			f, p := frozenFixture(t)
			ctx := context.Background()
			f.svc.save.WithFrozenMemory(nil, memory.NewRuleDistiller())
			before := readFrozenMemorySelection(t, f)
			p = prepareFrozenMemoryReceipt(t, f, p)
			plan := *p.Outcomes[0].MemoryPlan
			if _, err := f.store.GetSnapshot(ctx, plan.Snapshot); !errors.Is(err, domain.ErrNotFound) {
				t.Fatal("preparation must precede the first live snapshot creation", err)
			}
			if plan.ExpectedMemory == "" || plan.ExpectedMemory == plan.Memory {
				t.Fatalf("expected separate imported A and prepared B: %+v", plan)
			}

			// The fixture removed the provider file after FreezeInput. Recreate
			// the exact transcript so the real live path, not a seeded snapshot,
			// wins first creation between preparation and frozen attachment.
			source := f.source(t, "frozen", "only the frozen message")
			live, err := f.svc.save.Save(ctx, inbound.SaveInput{Cwd: f.root, Provider: source.Provider,
				SessionPath: source.Path, Pending: true, Message: domain.HookMessagePrefix + " live capture",
				DocIdentity: p.Outcomes[0].Input.DocIdentity})
			if err != nil {
				t.Fatal("live pending save", err)
			}
			if live.SnapshotID != plan.Snapshot || live.SessionID != source.SessionID {
				t.Fatalf("live and frozen capture did not deduplicate: live=%+v plan=%+v", live, plan)
			}
			snap, err := f.store.GetSnapshot(ctx, live.SnapshotID)
			if err != nil || snap.MemoryHash != plan.ExpectedMemory {
				t.Fatalf("live Save imported a different A: got=%s want=%s err=%v", snap.MemoryHash, plan.ExpectedMemory, err)
			}
			imported, err := f.store.GetMemory(ctx, snap.MemoryHash)
			if err != nil || imported.PreviousMemoryHash != "" || !strings.Contains(imported.Summary, "ORIGINAL MEMORY") || strings.Contains(imported.Summary, "only the frozen message") {
				t.Fatal("live A is not the original imported baseline", err)
			}
			prepared, err := f.store.GetMemory(ctx, plan.Memory)
			if err != nil || prepared.PreviousMemoryHash != snap.MemoryHash || !strings.Contains(prepared.Summary, "ORIGINAL MEMORY") || !strings.Contains(prepared.Summary, "only the frozen message") {
				t.Fatal("prepared B does not extend live A with the frozen contribution", err)
			}
			pendings, err := f.store.ListPendings(ctx, f.git.repo.ID)
			if err != nil || len(pendings) != 1 || pendings[0].Target != plan.Snapshot || pendings[0].SessionID != live.SessionID {
				t.Fatal("live Save did not create the expected pending capture", err)
			}
			requireFrozenMemorySelection(t, f, before)
			if err := os.Remove(source.Path); err != nil {
				t.Fatal(err)
			}

			if competing {
				c, err := f.store.PutMemory(ctx, domain.MemoryDigest{SnapshotID: plan.Snapshot,
					PreviousMemoryHash: plan.ExpectedMemory, Summary: "unrelated C after live A"})
				if err != nil {
					t.Fatal(err)
				}
				if err := f.store.CompareAndSwapSnapshotMemory(ctx, plan.Snapshot, plan.ExpectedMemory, c); err != nil {
					t.Fatal(err)
				}
				if out, err := f.svc.save.SaveFrozen(ctx, f.root, p, 0); !errors.Is(err, domain.ErrSyncConflict) {
					t.Fatalf("compatibility with live A accepted unrelated C: out=%+v err=%v", out, err)
				}
				snap, err = f.store.GetSnapshot(ctx, plan.Snapshot)
				if err != nil || snap.MemoryHash != c {
					t.Fatal("frozen replay overwrote competing C", err)
				}
				events, err := f.store.ListHistoryEvents(ctx, f.git.repo.ID)
				if err != nil {
					t.Fatal(err)
				}
				for _, event := range events {
					if event.ID == domain.CaptureProviderObservationID(p.Proof.ID, 0) || event.ID == domain.CaptureFinalObservationID(p.Proof.ID) {
						t.Fatal("conflicting capture recorded successful frozen memory")
					}
				}
				requireFrozenMemorySelection(t, f, before)
				return
			}

			out, err := f.svc.save.SaveFrozen(ctx, f.root, p, 0)
			if err != nil || out.SnapshotID != live.SnapshotID || out.MemoryHash != plan.Memory || out.MemorySource != plan.Snapshot {
				t.Fatalf("frozen B could not follow live A: out=%+v err=%v", out, err)
			}
			p.Outcomes[0].State, p.Outcomes[0].Target = "saved", out.SnapshotID
			p.Outcomes[0].MemoryHash, p.Outcomes[0].MemorySource = out.MemoryHash, out.MemorySource
			p.Proof.Source, p.Proof.Target = out.SnapshotID, out.SnapshotID
			final, err := f.svc.save.PrepareFrozenMemory(ctx, f.root, p, -1)
			if err != nil || final == nil {
				t.Fatal("prepare final frozen memory", err)
			}
			if final.Memory != plan.Memory || final.ExpectedMemory != plan.Memory || final.Snapshot != plan.Snapshot {
				t.Fatalf("single-provider finalization changed prepared B: %+v", final)
			}
			p.FinalMemory = final
			if err := p.Validate(); err != nil {
				t.Fatal("invalid final memory receipt", err)
			}
			event, err := f.svc.save.FinishFrozenMemory(ctx, f.root, p)
			if err != nil || event.ID != domain.CaptureFinalObservationID(p.Proof.ID) || event.MemoryHash != plan.Memory || event.MemorySource != plan.Snapshot || !event.MemoryPinned {
				t.Fatalf("final observation did not pin B: event=%+v err=%v", event, err)
			}
			again, err := f.svc.save.FinishFrozenMemory(ctx, f.root, p)
			if err != nil || !reflect.DeepEqual(event, again) {
				t.Fatal("final memory retry changed its observation", err)
			}
			snap, err = f.store.GetSnapshot(ctx, plan.Snapshot)
			if err != nil || snap.MemoryHash != plan.Memory {
				t.Fatal("final attachment does not retain B", err)
			}
			events, err := f.store.ListHistoryEvents(ctx, f.git.repo.ID)
			if err != nil || !slices.ContainsFunc(events, func(stored domain.HistoryEvent) bool { return reflect.DeepEqual(stored, event) }) {
				t.Fatal("final B observation was not durable", err)
			}
			requireFrozenMemorySelection(t, f, before)
		})
	}
}

func TestFrozenMemoryCompetingAttachmentPreservesConflict(t *testing.T) {
	f, p := frozenFixture(t)
	ctx := context.Background()
	f.svc.save.WithFrozenMemory(nil, memory.NewRuleDistiller())
	target, a := seedFrozenMemoryAttachment(t, f, p)
	p = prepareFrozenMemoryReceipt(t, f, p)
	plan := *p.Outcomes[0].MemoryPlan
	if plan.ExpectedMemory != a {
		t.Fatalf("plan must expect A: %+v", plan)
	}
	// C is another child of A, not a descendant of the prepared B.
	c, err := f.store.PutMemory(ctx, domain.MemoryDigest{SnapshotID: target, PreviousMemoryHash: a, Summary: "unrelated concurrent C"})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.CompareAndSwapSnapshotMemory(ctx, target, a, c); err != nil {
		t.Fatal(err)
	}
	before := readFrozenMemorySelection(t, f)
	for attempt := 0; attempt < 2; attempt++ {
		out, err := f.svc.save.SaveFrozen(ctx, f.root, p, 0)
		if !errors.Is(err, domain.ErrSyncConflict) {
			t.Fatalf("attempt %d silently rebased/overwrote competing C: out=%+v err=%v", attempt, out, err)
		}
		snap, err := f.store.GetSnapshot(ctx, target)
		if err != nil || snap.MemoryHash != c {
			t.Fatal("conflicting replay replaced C", err)
		}
		if *p.Outcomes[0].MemoryPlan != plan {
			t.Fatal("conflicting replay rewrote the frozen CAS plan")
		}
		requireFrozenMemorySelection(t, f, before)
	}
	events, err := f.store.ListHistoryEvents(ctx, f.git.repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		if e.ID == domain.CaptureProviderObservationID(p.Proof.ID, 0) {
			t.Fatalf("failed attachment recorded a successful frozen observation: %+v", e)
		}
	}
}

func TestFrozenMemoryRetryUnderCausalDescendantKeepsExactPlan(t *testing.T) {
	f, p := frozenFixture(t)
	ctx := context.Background()
	f.svc.save.WithFrozenMemory(nil, memory.NewRuleDistiller())
	target, a := seedFrozenMemoryAttachment(t, f, p)
	p = prepareFrozenMemoryReceipt(t, f, p)
	b := p.Outcomes[0].MemoryPlan.Memory
	prepared, err := f.store.GetMemory(ctx, b)
	if err != nil || prepared.PreviousMemoryHash != a {
		t.Fatal("prepared B must causally follow A", err)
	}
	before := readFrozenMemorySelection(t, f)
	out, err := f.svc.save.SaveFrozen(ctx, f.root, p, 0)
	if err != nil || out.MemoryHash != b {
		t.Fatal("attach prepared B", out, err)
	}
	c, err := f.store.PutMemory(ctx, domain.MemoryDigest{SnapshotID: target, PreviousMemoryHash: b, Summary: "later causal C"})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.CompareAndSwapSnapshotMemory(ctx, target, b, c); err != nil {
		t.Fatal(err)
	}
	// Simulate the lost acknowledgement: replay the same journaled B, with no
	// new preparation against the now-current C attachment.
	again, err := f.svc.save.SaveFrozen(ctx, f.root, p, 0)
	if err != nil || again != out {
		t.Fatalf("retry did not return the original B result: before=%+v after=%+v err=%v", out, again, err)
	}
	snap, err := f.store.GetSnapshot(ctx, target)
	if err != nil || snap.MemoryHash != c {
		t.Fatal("retry rolled the attachment back from C to B", err)
	}
	stillB, err := f.store.GetMemory(ctx, b)
	if err != nil || !reflect.DeepEqual(prepared, stillB) {
		t.Fatal("retry rewrote prepared memory B", err)
	}
	events, err := f.store.ListHistoryEvents(ctx, f.git.repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, e := range events {
		if e.ID != domain.CaptureProviderObservationID(p.Proof.ID, 0) {
			continue
		}
		found++
		if e.MemoryHash != b || e.MemorySource != target || e.Target != target || !e.MemoryPinned {
			t.Fatalf("historical observation adopted the later C attachment: %+v", e)
		}
	}
	if found != 1 {
		t.Fatalf("expected one durable B observation after retry, got %d", found)
	}
	requireFrozenMemorySelection(t, f, before)
}

func TestFrozenMemoryImportedFragmentsStayPinnedWithoutCompleteCoverage(t *testing.T) {
	f, p := frozenFixture(t)
	ctx := context.Background()
	f.svc.save.WithFrozenMemory(nil, memory.NewRuleDistiller())
	p = prepareFrozenMemoryReceipt(t, f, p)
	plan := p.Outcomes[0].MemoryPlan
	digest, err := f.store.GetMemory(ctx, plan.Memory)
	if err != nil {
		t.Fatal(err)
	}
	imported := false
	for _, fragment := range digest.Fragments {
		if fragment.SourceSnapshot == p.Initial && strings.Contains(fragment.Summary, "ORIGINAL MEMORY") {
			imported = true
		}
	}
	if !imported {
		t.Fatal("frozen baseline lost its imported-fragment provenance")
	}
	coverage := digest.GraftCoverage
	if coverage == nil || !slices.Contains(coverage.PinnedSources, p.Initial) {
		t.Fatal("frozen imported baseline is not pinned")
	}
	if coverage.ProjectionComplete || coverage.LineageFingerprint != "" {
		t.Fatalf("frozen imports falsely claim complete mutable-graph coverage: %+v", coverage)
	}
	if slices.Contains(coverage.PinnedSources, plan.Snapshot) {
		t.Fatal("fresh self-owned contribution mislabeled as an imported source")
	}
	out, err := f.svc.save.SaveFrozen(ctx, f.root, p, 0)
	if err != nil || out.MemoryHash != plan.Memory {
		t.Fatal("save changed frozen imported memory", err)
	}
	stored, err := f.store.GetMemory(ctx, out.MemoryHash)
	if err != nil || !reflect.DeepEqual(digest, stored) {
		t.Fatal("attachment changed the prepared import coverage", err)
	}
}

func TestFrozenMemoryRejectsSettingsCorruptionAfterPreparation(t *testing.T) {
	for _, kind := range []string{"claude", "agents", "codex"} {
		t.Run(kind, func(t *testing.T) {
			f, p := frozenFixture(t)
			ctx := context.Background()
			f.svc.save.WithFrozenMemory(nil, memory.NewRuleDistiller())
			target, a := seedFrozenMemoryAttachment(t, f, p)
			bundle := domain.SettingsBundle{Kind: kind, Files: []domain.SettingsFile{{Path: "synthetic.txt", ContentB64: base64.StdEncoding.EncodeToString([]byte("frozen settings"))}}}
			hash, err := f.store.PutSettingsObject(ctx, bundle)
			if err != nil {
				t.Fatal(err)
			}
			p.Settings = map[string]domain.ContentHash{kind: hash}
			p = prepareFrozenMemoryReceipt(t, f, p)
			before := readFrozenMemorySelection(t, f)

			// Replace valid JSON with different valid settings bytes under the old
			// content hash. Save must reverify even on the existing-snapshot path.
			bundle.Files[0].ContentB64 = base64.StdEncoding.EncodeToString([]byte("corrupted later settings"))
			raw, err := json.Marshal(bundle)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(f.root, ".cxt", "objects", "settingsobjs", strings.TrimPrefix(string(hash), "sha256:"))
			if err := os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := f.store.GetSettingsObject(ctx, hash); !errors.Is(err, domain.ErrHashMismatch) {
				t.Fatal("fixture did not corrupt the frozen settings object", err)
			}
			out, err := f.svc.save.SaveFrozen(ctx, f.root, p, 0)
			if !errors.Is(err, domain.ErrHashMismatch) {
				t.Errorf("frozen save accepted corrupted %s settings: out=%+v err=%v", kind, out, err)
			}
			snap, err := f.store.GetSnapshot(ctx, target)
			if err != nil || snap.MemoryHash != a {
				t.Error("corrupted settings allowed prepared memory to attach", err)
			}
			events, err := f.store.ListHistoryEvents(ctx, f.git.repo.ID)
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range events {
				if e.ID == domain.CaptureProviderObservationID(p.Proof.ID, 0) {
					t.Errorf("corrupted settings produced a successful capture observation: %+v", e)
				}
			}
			requireFrozenMemorySelection(t, f, before)
		})
	}
}
