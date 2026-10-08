package app

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
)

// Pending snapshot metadata selects the reader identity. A body alone must not
// bypass that boundary, and a complete fixture must actually reach its body.
func TestWorkingHistoryObservationPendingSnapshotSelectsBody(t *testing.T) {
	for _, root := range []bool{false, true} {
		for _, missing := range []bool{false, true} {
			t.Run(fmt.Sprintf("root=%t/missing=%t", root, missing), func(t *testing.T) {
				s, f, _, catalog := newWorkingHistoryObservationFixture(t)
				doc := workingDoc(t, "session", 3)
				if root {
					doc = contextRootDoc(t, f, 3)
					f.pending[0].Target = doc.Hash
				}
				reader := &rootContextDiffReader{workingReadFixture: f}
				s.docs = reader
				if missing {
					delete(f.snapshots, doc.Hash)
				}
				bodyReads := 0
				f.onDoc = func() { bodyReads++ }
				out, err := s.Diff(context.Background(), inbound.ContextDiffInput{})
				if err != nil || len(out.Changes) != 1 || catalog.reads != 3 || catalog.refReads != 3 {
					t.Fatalf("unexpected query/fence: result=%+v scans=%d/%d err=%v", out, catalog.reads, catalog.refReads, err)
				}
				change := out.Changes[0]
				if missing {
					if change.Reason != "pending_snapshot_missing" || change.State != "unavailable" || change.CountsKnown || bodyReads != 0 || len(reader.reads) != 0 {
						t.Fatalf("body bypassed missing snapshot: %+v reads=%d refs=%v", change, bodyReads, reader.reads)
					}
					return
				}
				if change.State != "extended" || !change.CountsKnown || change.After != doc.Hash || change.AfterIdentity != doc.Identity || change.Added.Start != 2 || change.Added.End != 3 || bodyReads != 2 || len(reader.reads) != 2 {
					t.Fatalf("complete pending capture did not reach exact bodies: %+v reads=%d refs=%v", change, bodyReads, reader.reads)
				}
				if reader.reads[0] != doc.DocumentRef() || reader.reads[1] != f.index.Entries[0].DocumentRef() {
					t.Fatalf("snapshot/staging identity was lost: %v", reader.reads)
				}
			})
		}
	}
}

// Keep the original legacy matrix and additionally exercise its post-body
// fences through the optional explicit-reference reader with a root pending.
func TestWorkingHistoryObservationExplicitReaderPostBodyFence(t *testing.T) {
	for _, fallback := range []bool{false, true} {
		mutations := []string{"pending", "catalog-error"}
		if !fallback {
			// Only the observer exposes the private full-catalog revision.
			mutations = append(mutations, "hidden", "continuous")
		}
		for _, mutation := range mutations {
			t.Run(fmt.Sprintf("fallback=%t/%s", fallback, mutation), func(t *testing.T) {
				s, f, history, catalog := newWorkingHistoryObservationFixture(t)
				initial, replacement := contextRootDoc(t, f, 3), contextRootDoc(t, f, 4)
				f.pending[0].Target = initial.Hash
				reader := &rootContextDiffReader{workingReadFixture: f}
				s.docs = reader
				scans := 1
				if fallback {
					s.history = workingHistoryQueryOnly{HistoryQuery: history}
					scans = 2
				}
				bodyReads := 0
				catalogErr := errors.New("post-body catalog unavailable")
				f.onDoc = func() {
					bodyReads++
					if catalog.reads < 2*scans {
						t.Fatal("body opened before matching observations")
					}
					if mutation != "continuous" && bodyReads != 1 {
						return
					}
					switch mutation {
					case "pending":
						f.pending[0].Target = replacement.Hash
					case "hidden", "continuous":
						catalog.snaps[3].Message = fmt.Sprintf("PRIVATE_DO_NOT_PRINT_%d", bodyReads)
					case "catalog-error":
						catalog.refErr = catalogErr
					}
				}
				out, err := s.Diff(context.Background(), inbound.ContextDiffInput{})
				if mutation == "catalog-error" {
					if !errors.Is(err, catalogErr) || bodyReads != 2 || catalog.reads != 2*scans+1 || catalog.refReads != 2*scans+1 {
						t.Fatalf("post-body catalog failure lost: reads=%d scans=%d/%d err=%v", bodyReads, catalog.reads, catalog.refReads, err)
					}
					return
				}
				if mutation == "continuous" {
					if !errors.Is(err, domain.ErrSelectionChanged) || bodyReads != 6 || catalog.reads != 9*scans || catalog.refReads != 9*scans {
						t.Fatalf("continuous mutation escaped bounded fence: reads=%d scans=%d/%d err=%v", bodyReads, catalog.reads, catalog.refReads, err)
					}
					return
				}
				if err != nil || len(out.Changes) != 1 || bodyReads != 4 || catalog.reads != 6*scans || catalog.refReads != 6*scans {
					t.Fatalf("post-body mutation did not rebuild: reads=%d scans=%d/%d err=%v", bodyReads, catalog.reads, catalog.refReads, err)
				}
				want, events := initial, 3
				if mutation == "pending" {
					want, events = replacement, 4
				}
				change := out.Changes[0]
				if change.After != want.Hash || change.AfterIdentity != want.Identity || change.State != "extended" || change.Added.Start != 2 || change.Added.End != events {
					t.Fatalf("mixed pending identity or body: %+v", change)
				}
				f.onDoc = nil
				settled, err := s.Diff(context.Background(), inbound.ContextDiffInput{})
				if err != nil {
					t.Fatal(err)
				}
				assertWorkingHistoryObservationPublicEqual(t, out, settled)
			})
		}
	}
}
