//go:build postgres

package app

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type historyPublicationReplayProbePG struct {
	*historyDocumentLockProbe
	afterRead func() error
}

func (probe *historyPublicationReplayProbePG) WithinReadSnapshot(ctx context.Context, read func(context.Context) error) error {
	if err := probe.PostgresStore.WithinReadSnapshot(ctx, read); err != nil {
		return err
	}
	if probe.afterRead != nil {
		return probe.afterRead()
	}
	return nil
}

func TestPGHistoryDocumentPreparationPublicationWriterReplays(test *testing.T) {
	for _, writer := range []string{"message_replay", "message_promotion", "message_conflict", "snapshot_replay", "document_replay", "chunk_replay"} {
		test.Run(writer, func(test *testing.T) {
			service, base, event := historyDocumentLockFixture(test)
			probe := &historyPublicationReplayProbePG{historyDocumentLockProbe: base}
			service.meta, service.blobs, service.repositories = probe, probe, probe
			ctx := systemTestContext()
			repo := domain.ContentHash(event.RepoID)
			const message = "first-source [git abc1234]"
			if _, err := probe.peer.Exec(ctx, `UPDATE snapshots SET message=$3 WHERE repo_id=$1 AND id=$2`, repo, event.Target, domain.HookMessagePrefix+"checkpoint"); err != nil {
				test.Fatal(err)
			}
			if writer != "message_promotion" {
				if err := service.PromoteSnapshotMessage(ctx, repo, event.Target, message); err != nil {
					test.Fatal(err)
				}
			}
			snapshot, err := probe.GetSnapshot(ctx, repo, event.Target)
			if err != nil {
				test.Fatal(err)
			}
			document, err := probe.GetDoc(ctx, repo, snapshot.DocHash)
			if err != nil {
				test.Fatal(err)
			}
			canonical, err := domain.CanonicalBytes(document.CIR)
			if err != nil {
				test.Fatal(err)
			}
			chunks, chunked := domain.PlanDocChunks(canonical)
			if !chunked {
				test.Fatal("publication fixture must include chunk grants")
			}
			versions := func() ([3]string, error) {
				var current [3]string
				err := probe.peer.QueryRow(ctx, `SELECT
 (SELECT xmin::text||':'||ctid::text FROM snapshots WHERE repo_id=$1 AND id=$2),
 (SELECT string_agg(hash||':'||xmin::text||':'||ctid::text,',' ORDER BY hash) FROM blobs WHERE hash IN (SELECT hash FROM repo_blobs WHERE repo_id=$1)),
 (SELECT string_agg(kind||':'||hash||':'||xmin::text||':'||ctid::text,',' ORDER BY kind,hash) FROM repo_blobs WHERE repo_id=$1)`, repo, event.Target).Scan(&current[0], &current[1], &current[2])
				return current, err
			}
			before, err := versions()
			if err != nil {
				test.Fatal(err)
			}
			writes := 0
			probe.afterRead = func() error {
				writes++
				if len(probe.observations) != 1 || probe.applies != 0 {
					return fmt.Errorf("writer did not run between preparation and admission")
				}
				switch writer {
				case "message_replay", "message_promotion":
					return service.PromoteSnapshotMessage(ctx, repo, event.Target, message)
				case "message_conflict":
					if err := service.PromoteSnapshotMessage(ctx, repo, event.Target, "different commit"); !errors.Is(err, domain.ErrConflict) {
						return fmt.Errorf("conflicting promotion returned %v", err)
					}
					return nil
				case "snapshot_replay":
					return probe.PutSnapshot(ctx, snapshot)
				case "document_replay":
					_, err := probe.PutDoc(ctx, repo, document)
					return err
				case "chunk_replay":
					_, _, err := probe.PutChunks(ctx, repo, chunks.Bodies)
					return err
				}
				return errors.New("unknown publication writer")
			}
			historyErr := service.RecordHistory(ctx, event)
			after, err := versions()
			if err != nil {
				test.Fatal(err)
			}
			changed := [3]bool{before[0] != after[0], before[1] != after[1], before[2] != after[2]}
			wantChanged := [3]bool{writer == "message_promotion", false, false}
			wantApplies := 1
			if writer == "message_promotion" {
				wantApplies = 0
				if !errors.Is(historyErr, domain.ErrConflict) {
					test.Fatalf("real promotion must invalidate captured proof: %v", historyErr)
				}
			} else if historyErr != nil {
				test.Errorf("idempotent publication invalidated history: %v; tuple changes [snapshots blobs repo_blobs]=%v", historyErr, changed)
			}
			if changed != wantChanged || writes != 1 || probe.applies != wantApplies || len(probe.observations) != 1 {
				test.Errorf("writer effects: tuple changes=%v want=%v writes=%d applies=%d want=%d verifies=%d", changed, wantChanged, writes, probe.applies, wantApplies, len(probe.observations))
			}
			events, err := probe.ListHistoryEvents(ctx, repo)
			if err != nil || len(events) != wantApplies {
				test.Fatalf("history admission: events=%d want=%d err=%v", len(events), wantApplies, err)
			}
			if wantApplies == 1 && !reflect.DeepEqual(events[0], event) {
				test.Fatal("accepted history differs from the submitted event")
			}
		})
	}
}
