package app

import (
	"context"
	"errors"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

func TestPushProgressNeverClaimsPublicationAfterFailure(t *testing.T) {
	for _, failure := range []bool{false, true} {
		svc, remote, root, _ := setupPushOrder(t)
		if failure {
			remote.failGraft = errors.New("unavailable")
		}
		var observed []inbound.SyncProgress
		_, err := svc.Push(context.Background(), inbound.SyncInput{Cwd: root, Progress: func(p inbound.SyncProgress) { observed = append(observed, p) }})
		if (err != nil) != failure {
			t.Fatal(err)
		}
		seenDocs, seenSnapshots, complete := false, false, false
		for _, p := range observed {
			if p.Completed < 0 || p.Completed > p.Total {
				t.Fatalf("invalid acknowledged counter: %+v", p)
			}
			switch p.Phase {
			case "upload-and-verify-documents":
				if p.Total > 0 && p.Completed == p.Total {
					seenDocs = true
				}
			case "publish-snapshots":
				if !seenDocs {
					t.Fatal("snapshot progress preceded document verification")
				}
				if p.Total > 0 && p.Completed == p.Total {
					seenSnapshots = true
				}
			case "publish-refs":
				if failure || !seenSnapshots {
					t.Fatal("ref progress claimed a failed or unverified publication")
				}
			case "complete":
				complete = true
			}
		}
		if complete == failure {
			t.Fatalf("completion=%v failure=%v", complete, failure)
		}
	}
}

type failedProgressDocuments struct{ outbound.RemoteSync }

func (r failedProgressDocuments) Push(ctx context.Context, repo string, snaps []domain.Snapshot, docs []domain.SessionDoc, refs []domain.Ref, force, appendDiverged bool) error {
	if len(docs) > 0 {
		return errors.New("document finalization remains queued")
	}
	return r.RemoteSync.Push(ctx, repo, snaps, docs, refs, force, appendDiverged)
}

func TestPushProgressDoesNotCountUnacknowledgedDocuments(t *testing.T) {
	svc, _, root, _ := setupPushOrder(t)
	svc.remote = failedProgressDocuments{svc.remote}
	_, err := svc.Push(context.Background(), inbound.SyncInput{Cwd: root, Progress: func(p inbound.SyncProgress) {
		if p.Phase == "complete" || p.Phase == "publish-refs" || p.Phase == "publish-snapshots" || p.Phase == "upload-and-verify-documents" && p.Completed > 0 {
			t.Fatalf("unacknowledged publication reported: %+v", p)
		}
	}})
	if err == nil {
		t.Fatal("failed document publication succeeded")
	}
}

func TestCancelledPushDoesNotReportCompletion(t *testing.T) {
	svc, _, root, _ := setupPushOrder(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := svc.Push(ctx, inbound.SyncInput{Cwd: root, Progress: func(p inbound.SyncProgress) {
		if p.Phase == "complete" {
			t.Fatal("cancelled sync reported complete")
		}
	}})
	if err == nil {
		t.Fatal("cancelled sync succeeded")
	}
}
