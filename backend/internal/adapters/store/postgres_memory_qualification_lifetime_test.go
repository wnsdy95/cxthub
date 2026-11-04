//go:build postgres

package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func TestPGMemoryQualificationFinalExpiryAndSnapshotLifetime(test *testing.T) {
	for _, mode := range []string{"expiry", "cancel", "prune", "revoke"} {
		test.Run(mode, func(test *testing.T) {
			storage, parent, qualification, _ := memoryQualificationPG(test)
			repo := qualification.Source.Graph.RepoID
			identifier, err := storage.QualifyMemoryRead(parent, qualification)
			if err != nil {
				test.Fatal(err)
			}
			if mode == "expiry" {
				memoryReadPGExpireSoon(test, storage, parent, repo, qualification.CaptureID)
			}
			ctx, cancel := context.WithCancel(parent)
			defer cancel()
			err = storage.WithQualifiedMemoryReadRange(ctx, repo, identifier, memoryQualificationAllowPG, func(bound context.Context, _ domain.MemoryReadHeader, chunks outbound.MemoryReadChunkReader) error {
				switch mode {
				case "prune":
					catalogExecPG(test, parent, storage.pool, `DELETE FROM memory_capture_headers WHERE repo_id=$1 AND capture_id=$2`, string(repo), string(qualification.CaptureID))
				case "revoke":
					catalogExecPG(test, parent, storage.pool, `DELETE FROM repo_blobs WHERE repo_id=$1 AND kind='memory' AND hash=$2`, string(repo), string(qualification.Witnesses[0].Hash))
				}
				if _, err := chunks.ReadChunk(bound, 0); err != nil {
					return err
				}
				if mode == "expiry" {
					memoryReadPGWaitExpiry(test, storage, parent, repo, qualification.CaptureID)
				} else if mode == "cancel" {
					cancel()
				}
				return nil
			})
			switch mode {
			case "expiry":
				if !errors.Is(err, domain.ErrMemoryCaptureExpired) {
					test.Fatal("read completed after its retention expired", err)
				}
			case "cancel":
				if !errors.Is(err, context.Canceled) {
					test.Fatal("read ignored cancellation", err)
				}
			default:
				if err != nil {
					test.Fatal("coherent in-flight snapshot was fractured", err)
				}
				if err := storage.WithQualifiedMemoryReadRange(parent, repo, identifier, memoryQualificationAllowPG, func(context.Context, domain.MemoryReadHeader, outbound.MemoryReadChunkReader) error {
					test.Fatal("new snapshot accepted deleted source")
					return nil
				}); err == nil {
					test.Fatal("new snapshot accepted deleted source")
				}
			}
		})
	}
}

func TestPGMemoryQualificationExpiryBetweenCheckAndCommit(test *testing.T) {
	storage, parent, qualification, _ := memoryQualificationPG(test)
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	repo := qualification.Source.Graph.RepoID
	peer, reached, release := memoryQualificationPausePG(test, ctx, storage, "SELECT expires_at>clock_timestamp()")
	defer release()
	memoryReadPGExpireSoon(test, storage, ctx, repo, qualification.CaptureID)
	done := make(chan error, 1)
	go func() { _, err := peer.QualifyMemoryRead(ctx, qualification); done <- err }()
	memoryQualificationWaitPG(test, ctx, reached, done)
	memoryReadPGWaitExpiry(test, storage, ctx, repo, qualification.CaptureID)
	release()
	err := <-done
	if !errors.Is(err, domain.ErrMemoryCaptureExpired) {
		test.Fatal("deferred live-input check allowed expired commit", err)
	}
	if memoryQualificationCountPG(test, storage, ctx, repo) != 0 {
		test.Fatal("failed commit retained a receipt")
	}
}
