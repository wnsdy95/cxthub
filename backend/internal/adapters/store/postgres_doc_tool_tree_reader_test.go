//go:build postgres

package store

import (
	"context"
	"errors"
	"runtime"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func completeDocToolTreePG(test *testing.T, fixture docDisplayPGFixture, job domain.DocDisplayJob, prepared domain.DocEventDisplayPreparation) domain.DocEventDisplayPreparation {
	test.Helper()
	started := time.Now()
	for prepared.Phase == "tool_tree" {
		step := docEventDisplayBoundaryStep(test, fixture, job, prepared)
		var err error
		prepared, err = fixture.storage.SaveDocEventDisplay(fixture.ctx, job, step)
		if err != nil {
			var state, preparation string
			var version int64
			var live bool
			var lease, server time.Time
			diagnostic := fixture.storage.pool.QueryRow(fixture.ctx, `SELECT state,version,coalesce(preparation_hash,''),lease_until>clock_timestamp(),lease_until,clock_timestamp()
 FROM doc_display_jobs_v1 WHERE repo_id=$1 AND doc_hash=$2 AND event_index=$3 AND profile=$4`,
				job.RepoID, job.DocHash, job.Target.Event.Index, job.Target.DisplayProfile()).Scan(&state, &version, &preparation, &live, &lease, &server)
			client := time.Now()
			test.Fatalf("tree save: err=%v claim_version=%d expected_preparation=%s current_state=%s current_version=%d current_preparation=%s server_lease_live=%t lease_utc=%s server_utc=%s client_utc=%s claim_created_utc=%s monotonic_elapsed=%s wall_elapsed=%s diagnostic=%v", err, job.Version, step.Prior(), state, version, preparation, live, lease.UTC().Format(time.RFC3339Nano), server.UTC().Format(time.RFC3339Nano), client.UTC().Format(time.RFC3339Nano), job.CreatedAt.UTC().Format(time.RFC3339Nano), client.Sub(started), client.Round(0).Sub(started.Round(0)), diagnostic)
		}
	}
	if prepared.Phase != "tool_tree_done" {
		test.Fatal("tree stage did not complete", prepared.Phase)
	}
	return prepared
}

func readDocToolTreePG(test *testing.T, fixture docDisplayPGFixture, job domain.DocDisplayJob, prepared domain.DocEventDisplayPreparation, check func(context.Context, outbound.DocToolTreeWorkReader)) {
	test.Helper()
	encoded, err := prepared.Encode()
	if err != nil {
		test.Fatal(err)
	}
	if err := fixture.storage.WithinReadSnapshot(fixture.ctx, func(read context.Context) error {
		work, err := fixture.storage.OpenDocEventDisplayWork(read, job, domain.HashContent(encoded))
		if err != nil {
			return err
		}
		reader, found := work.(outbound.DocToolTreeWorkReader)
		if !found {
			test.Fatal("actual work reader has no private tool-tree capability")
		}
		check(read, reader)
		return nil
	}); err != nil {
		test.Fatal(err)
	}
}

func TestDocToolTreePGReaderCompletedKeysetAndScope(test *testing.T) {
	fixture, job, prepared := docToolTreePGFixture(test, 14)
	step := docEventDisplayBoundaryStep(test, fixture, job, prepared)
	var err error
	prepared, err = fixture.storage.SaveDocEventDisplay(fixture.ctx, job, step)
	if err != nil {
		test.Fatal(err)
	}
	readDocToolTreePG(test, fixture, job, prepared, func(read context.Context, reader outbound.DocToolTreeWorkReader) {
		if nodes, err := reader.ReadToolTreeNodes(read, *prepared.ToolTree, -1, 7); !errors.Is(err, domain.ErrDocRangeIndexUnavailable) || nodes != nil {
			test.Fatal("partially scanned source was readable", nodes, err)
		}
	})
	prepared = completeDocToolTreePG(test, fixture, job, prepared)
	before := docToolTreePGImage(test, fixture, job)
	var escaped outbound.DocToolTreeWorkReader
	var escapedContext context.Context
	readDocToolTreePG(test, fixture, job, prepared, func(read context.Context, reader outbound.DocToolTreeWorkReader) {
		escaped, escapedContext = reader, read
		after, total, calls := -1, 0, 0
		for {
			nodes, err := reader.ReadToolTreeNodes(read, *prepared.ToolTree, after, 7)
			if err != nil || len(nodes) > 7 {
				test.Fatal("bounded keyset read failed", err, len(nodes))
			}
			calls++
			if len(nodes) == 0 {
				break
			}
			for _, node := range nodes {
				if node.Validate(*prepared.ToolTree) != nil || node.Value.From <= after {
					test.Fatal("keyset duplicated or returned invalid nodes", node)
				}
				after = node.Value.From
				total++
			}
		}
		if total != 14*9+3 || calls < 10 {
			test.Fatal("keyset omitted nodes", total, calls)
		}
		for _, limit := range []int{0, -1, 65} {
			if nodes, err := reader.ReadToolTreeNodes(read, *prepared.ToolTree, -1, limit); !errors.Is(err, domain.ErrValidation) || nodes != nil {
				test.Fatal("invalid work limit was accepted", limit, nodes, err)
			}
		}
		for _, after := range []int{-2, prepared.ToolTree.Root.To} {
			if nodes, err := reader.ReadToolTreeNodes(read, *prepared.ToolTree, after, 7); !errors.Is(err, domain.ErrValidation) || nodes != nil {
				test.Fatal("invalid work cursor was accepted", after, nodes, err)
			}
		}
		foreign := *prepared.ToolTree
		foreign.Evidence = domain.HashContent([]byte("foreign source"))
		if nodes, err := reader.ReadToolTreeNodes(read, foreign, -1, 7); !errors.Is(err, domain.ErrConflict) || nodes != nil {
			test.Fatal("foreign source reused the tree capability", nodes, err)
		}
		cancelled, cancel := context.WithCancel(read)
		cancel()
		if nodes, err := reader.ReadToolTreeNodes(cancelled, *prepared.ToolTree, -1, 7); !errors.Is(err, context.Canceled) || nodes != nil {
			test.Fatal("cancelled read returned nodes", nodes, err)
		}
		if err := fixture.storage.WithinReadSnapshot(fixture.ctx, func(other context.Context) error {
			if nodes, err := reader.ReadToolTreeNodes(other, *prepared.ToolTree, -1, 7); !errors.Is(err, domain.ErrConflict) || nodes != nil {
				test.Fatal("tree reader escaped into another transaction", nodes, err)
			}
			return nil
		}); err != nil {
			test.Fatal(err)
		}
	})
	if nodes, err := escaped.ReadToolTreeNodes(fixture.ctx, *prepared.ToolTree, -1, 7); !errors.Is(err, domain.ErrConflict) || nodes != nil {
		test.Fatal("tree reader escaped without a read snapshot", nodes, err)
	}
	if nodes, err := escaped.ReadToolTreeNodes(escapedContext, *prepared.ToolTree, -1, 7); err == nil || nodes != nil {
		test.Fatal("closed read snapshot returned private nodes", nodes, err)
	}
	if docToolTreePGImage(test, fixture, job) != before {
		test.Fatal("private reads changed durable state")
	}
	assertDocEventDisplayBoundaryUnpublished(test, fixture, job)
}

func TestDocToolTreePGReaderRejectsCorruptionWithoutPartialRows(test *testing.T) {
	fixture, job, prepared := docToolTreePGFixture(test, 3)
	prepared = completeDocToolTreePG(test, fixture, job, prepared)
	scope, _ := prepared.ToolTree.Hash()
	var selected int
	if err := fixture.storage.pool.QueryRow(fixture.ctx, `SELECT node_from FROM doc_tool_tree_nodes_v1 WHERE scope=$1 ORDER BY node_from OFFSET 2 LIMIT 1`, scope).Scan(&selected); err != nil {
		test.Fatal(err)
	}
	if _, err := execDocToolCatalogPG(fixture, job, `UPDATE doc_tool_tree_nodes_v1 SET bytes=$3 WHERE scope=$1 AND node_from=$2`, scope, selected, []byte(`{}`)); err != nil {
		test.Fatal(err)
	}
	readDocToolTreePG(test, fixture, job, prepared, func(read context.Context, reader outbound.DocToolTreeWorkReader) {
		if nodes, err := reader.ReadToolTreeNodes(read, *prepared.ToolTree, -1, 7); !errors.Is(err, domain.ErrIntegrity) || nodes != nil {
			test.Fatal("corrupt selected row leaked earlier rows", nodes, err)
		}
	})
	assertDocEventDisplayBoundaryUnpublished(test, fixture, job)
}

func TestDocToolTreePGReaderRejectsNodeHashMismatch(test *testing.T) {
	fixture, job, prepared := docToolTreePGFixture(test, 1)
	prepared = completeDocToolTreePG(test, fixture, job, prepared)
	var before []domain.DocToolTreeNode
	readDocToolTreePG(test, fixture, job, prepared, func(read context.Context, reader outbound.DocToolTreeWorkReader) {
		var err error
		before, err = reader.ReadToolTreeNodes(read, *prepared.ToolTree, -1, 64)
		if err != nil {
			test.Fatal(err)
		}
	})
	scope, _ := prepared.ToolTree.Hash()
	if _, err := execDocToolCatalogPG(fixture, job, `UPDATE doc_tool_tree_nodes_v1 SET node_hash=$2 WHERE scope=$1 AND node_from=$3`, scope, domain.HashContent([]byte("wrong digest")), before[1].Value.From); err != nil {
		test.Fatal(err)
	}
	readDocToolTreePG(test, fixture, job, prepared, func(read context.Context, reader outbound.DocToolTreeWorkReader) {
		nodes, err := reader.ReadToolTreeNodes(read, *prepared.ToolTree, -1, 64)
		if !errors.Is(err, domain.ErrIntegrity) || nodes != nil {
			test.Fatal("node hash corruption was accepted", nodes, err)
		}
	})
}

func TestDocToolTreePGReaderOversizedHashHasBoundedTransfer(test *testing.T) {
	fixture, job, prepared := docToolTreePGFixture(test, 1)
	prepared = completeDocToolTreePG(test, fixture, job, prepared)
	scope, _ := prepared.ToolTree.Hash()
	var length int
	if err := fixture.storage.docDisplayWrite(fixture.ctx, job, time.Time{}, func(bound context.Context, transaction pgx.Tx) error {
		if err := bindDocToolCatalogCalculationPG(bound, transaction, job); err != nil {
			return err
		}
		return transaction.QueryRow(bound, `UPDATE doc_tool_tree_nodes_v1 SET node_hash=node_hash||repeat('x',$2)
 WHERE scope=$1 AND parent_from=-1 RETURNING octet_length(node_hash)`, scope, 16<<20).Scan(&length)
	}); err != nil || length < 16<<20 {
		test.Fatal("oversized hash fixture failed", length, err)
	}
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	readDocToolTreePG(test, fixture, job, prepared, func(read context.Context, reader outbound.DocToolTreeWorkReader) {
		if nodes, err := reader.ReadToolTreeNodes(read, *prepared.ToolTree, -1, 7); !errors.Is(err, domain.ErrIntegrity) || nodes != nil {
			test.Fatal("oversized hash was accepted or leaked rows", nodes, err)
		}
	})
	runtime.ReadMemStats(&after)
	allocated := after.TotalAlloc - before.TotalAlloc
	if allocated > 8<<20 {
		test.Fatal("oversized text reached the bounded read client", allocated)
	}
	test.Logf("stored_hash_bytes=%d read_client_allocated_bytes=%d", length, allocated)
}
