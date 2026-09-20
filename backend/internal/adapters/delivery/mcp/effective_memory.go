package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"unicode/utf8"
)

func (s *Server) SetEffectiveMemory(q inbound.EffectiveMemoryQuery) { s.effectiveMemory = q }

// Fragment only the transport representation. The shared application query
// owns selection, proof, status, continuation and the coherent generation.
func (s *Server) effectiveMemoryPage(ctx context.Context, repo domain.Repo, a toolArgs) (string, error) {
	if s.effectiveMemory == nil {
		return "", fmt.Errorf("effective memory query unavailable")
	}
	if domain.ValidateGitOID(a.CodeCommit) != nil {
		return "", fmt.Errorf("effective mode requires a full code_commit SHA")
	}
	cur, err := cursorFor(string(repo.ID), "memory_load", a)
	if err != nil {
		return "", err
	}
	if a.Cursor != "" && (cur.Version != 3 || cur.FragmentFormat != "memory-effective-v1" || cur.Projection == "" || cur.Snapshot == "") {
		return "", fmt.Errorf("invalid effective memory cursor")
	}
	if cur.Snapshot == "" {
		snap, err := s.resolveRef(ctx, repo, a.Ref)
		if err != nil {
			return "", err
		}
		cur.Snapshot = snap.ID
	}
	branch := ""
	if a.MemoryHash == "" {
		branch, err = s.memoryBranch(ctx, repo, a.Ref)
		if err != nil {
			return "", err
		}
	}
	out, err := s.effectiveMemory.QueryEffectiveMemory(ctx, repo.ID, domain.EffectiveMemoryRequest{Selection: domain.EffectiveMemorySelection{Branch: branch, SnapshotID: cur.Snapshot, CodeCommit: a.CodeCommit, MemoryHash: domain.ContentHash(a.MemoryHash)}, Limit: 1, Cursor: cur.EffectiveCursor})
	if err != nil {
		return "", err
	}
	if cur.Projection != "" && cur.Projection != out.StateHash {
		return "", fmt.Errorf("effective memory changed; restart memory_load without cursor")
	}
	cur.Version = 3
	cur.FragmentFormat = "memory-effective-v1"
	cur.Projection = out.StateHash
	result := map[string]any{"notice": archiveNotice, "mode": "effective", "selection": out.Selection, "state_hash": out.StateHash, "lineage_hash": out.LineageHash, "total": out.Total, "next_cursor": ""}
	if out.Inclusion != nil {
		result["inclusion"] = memoryInclusionSummary(out.Inclusion)
	}
	if len(out.Items) == 0 {
		if cur.Offset != 0 {
			return "", fmt.Errorf("invalid empty memory cursor")
		}
		result["item"] = nil
		return pageJSON(result)
	}
	raw, err := json.Marshal(out.Items[0])
	if err != nil {
		return "", err
	}
	if cur.Offset >= len(raw) || (cur.Offset > 0 && !utf8.RuneStart(raw[cur.Offset])) {
		return "", fmt.Errorf("invalid effective memory offset")
	}
	start := cur.Offset
	// Account for JSON escaping and cursor metadata, not only raw fragment bytes.
	for budget := pageBytes / 2; budget >= 4; budget /= 2 {
		end := fragmentEnd(raw, start, budget)
		next := cur
		next.Offset = end
		if end == len(raw) {
			next.Offset = 0
			next.EffectiveCursor = out.NextCursor
		}
		continuation := ""
		if end < len(raw) || out.NextCursor != "" {
			continuation = encodeCursor(next)
		}
		result["item_id"] = out.Items[0].ID
		result["byte_offset"] = start
		result["json_fragment"] = string(raw[start:end])
		result["item_complete"] = end == len(raw)
		result["next_cursor"] = continuation
		encoded, err := pageJSON(result)
		if err != nil {
			return "", err
		}
		if len(encoded) <= pageBytes {
			return encoded, nil
		}
	}
	return "", fmt.Errorf("effective memory metadata exceeds transport bound")
}
