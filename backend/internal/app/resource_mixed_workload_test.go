package app

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestResourceAdmissionMixedGraphVerification(t *testing.T) {
	svc, storage := newFsckSvc(t)
	svc.meta = &graphCacheReadState{FSStore: storage, readOnly: true}
	ctx, gate := admissionTestContext(resourceWaitLimit)
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	repo := hh(t.Name())
	view := domain.RepositoryView{Revision: domain.RepositoryRevision{Graph: 1}, DefaultBranch: "main"}
	for index := range 10000 {
		snapshot := domain.Snapshot{ID: hh(fmt.Sprintf("mixed-%d", index)), RepoID: repo, DocHash: hh("doc")}
		if index > 0 {
			snapshot.Parents = []domain.ContentHash{view.Snapshots[index-1].ID}
		}
		view.Snapshots = append(view.Snapshots, snapshot)
	}
	doc := domain.CIRDocument{Envelope: domain.CIREnvelope{CIRVersion: "2"}}
	for index := range 256 {
		doc.Events = append(doc.Events, domain.CIREvent{Kind: domain.EventMessage, Seq: index, Role: domain.RoleUser, Blocks: []domain.ContentBlock{{Type: "text", Text: strings.Repeat("x", 4096)}}})
	}
	raw, err := domain.CanonicalBytes(doc)
	if err != nil {
		t.Fatal(err)
	}
	digest := domain.HashContent(raw)
	var completed [3]atomic.Int64
	var overloaded atomic.Int64
	var workers sync.WaitGroup
	failures := make(chan error, 24)
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	started := time.Now()
	for index := range 24 {
		workers.Add(1)
		go func(kind int) {
			defer workers.Done()
			class := ResourceBulk
			if kind == 1 {
				class = ResourceInteractive
			}
			for {
				err := resourceWork(ctx, class, func(bound context.Context) error {
					if kind == 2 {
						_, err := svc.docVerifier.Verify(bound, digest, raw)
						return err
					}
					graph, err := svc.cachedGraphState(bound, repo, view, "")
					if err == nil && len(graph.SnapshotIDs) != len(view.Snapshots) {
						return errors.New("mixed workload changed graph contents")
					}
					return err
				})
				if errors.Is(err, domain.ErrResourceOverloaded) {
					overloaded.Add(1)
					select {
					case <-ctx.Done():
						failures <- ctx.Err()
						return
					case <-time.After(10 * time.Millisecond):
						continue
					}
				}
				if err != nil {
					failures <- err
				} else {
					completed[kind].Add(1)
				}
				return
			}
		}(index % 3)
	}
	workers.Wait()
	close(failures)
	for err := range failures {
		t.Error(err)
	}
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	assertResourcesReleased(t, gate)
	for kind := range completed {
		if completed[kind].Load() != 8 {
			t.Errorf("class %d completed %d/8", kind, completed[kind].Load())
		}
	}
	stats := svc.graphCache.Stats()
	if stats.Entries != 1 || stats.Bytes > graphProjectionCacheBytes {
		t.Fatalf("mixed workload cache: entries=%d bytes=%d", stats.Entries, stats.Bytes)
	}
	t.Logf("24 operations (8 graph/8 interactive graph/8 canonical verification), 10000 snapshots, doc=%d bytes, elapsed=%s, overload retries=%d, cache=%d bytes, total allocations=%d, final heap=%d; synthetic metadata, not end-to-end RSS", len(raw), time.Since(started), overloaded.Load(), stats.Bytes, after.TotalAlloc-before.TotalAlloc, after.HeapAlloc)
}
