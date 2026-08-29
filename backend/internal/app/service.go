package app

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

// Service implements all server inbound use-cases and HTTP read-through.
type Service struct {
	meta   outbound.MetadataStore
	blobs  outbound.BlobStore
	auth   outbound.AuthProvider
	engine outbound.GitEngine
	// ws is used for repo → workspace binding (remote URL path resolution). nil skips binding.
	ws outbound.WorkspaceStore
}

// NewService creates a Service with an outbound port injected.
func NewService(meta outbound.MetadataStore, blobs outbound.BlobStore, auth outbound.AuthProvider, engine outbound.GitEngine, ws outbound.WorkspaceStore) *Service {
	return &Service{meta: meta, blobs: blobs, auth: auth, engine: engine, ws: ws}
}

// Inbound port implementations are guaranteed at compile time.
var (
	_ inbound.CommitSnapshot = (*Service)(nil)
	_ inbound.ForkSnapshot   = (*Service)(nil)
	_ inbound.DiffSnapshots  = (*Service)(nil)
	_ inbound.UpdateRef      = (*Service)(nil)
	_ inbound.ListSnapshots  = (*Service)(nil)
	_ inbound.PushReceive    = (*Service)(nil)
	_ inbound.StoreChunks    = (*Service)(nil)
	_ inbound.PullChunks     = (*Service)(nil)
	_ inbound.PullSend       = (*Service)(nil)
	_ inbound.Authenticate   = (*Service)(nil)
)

// --- Helper ---

func difference(all, have []domain.ContentHash) []domain.ContentHash {
	haveSet := make(map[domain.ContentHash]bool, len(have))
	for _, h := range have {
		haveSet[h] = true
	}
	var out []domain.ContentHash
	for _, h := range all {
		if !haveSet[h] {
			out = append(out, h)
		}
	}
	return out
}

func short(h domain.ContentHash) string {
	hx := strings.TrimPrefix(string(h), "sha256:")
	if len(hx) > 8 {
		return hx[:8]
	}
	return hx
}

func validateHashes(hashes ...domain.ContentHash) error {
	for _, h := range hashes {
		if err := domain.ValidateContentHash(h); err != nil {
			return err
		}
	}
	return nil
}

func validateOptionalHashes(hashes ...domain.ContentHash) error {
	for _, h := range hashes {
		if err := domain.ValidateOptionalContentHash(h); err != nil {
			return err
		}
	}
	return nil
}

func validateSnapshotHashes(snap domain.Snapshot) error {
	if err := validateHashes(snap.RepoID, snap.ID, snap.DocHash); err != nil {
		return err
	}
	if snap.ID != snap.DocHash {
		return fmt.Errorf("%w: snapshot id must equal doc_hash", domain.ErrIntegrity)
	}
	if err := validateOptionalHashes(snap.MemoryHash, snap.ClaudeSettings, snap.AgentsSettings, snap.CodexSettings); err != nil {
		return err
	}
	for _, p := range snap.Parents {
		if err := domain.ValidateContentHash(p); err != nil {
			return err
		}
	}
	for _, p := range snap.GraftParents {
		if err := domain.ValidateContentHash(p); err != nil {
			return err
		}
	}
	return nil
}

func validateSessionDocHash(doc domain.SessionDoc) error {
	return domain.ValidateSessionDocHash(doc)
}

func validateSnapshotDocPair(snap domain.Snapshot, doc domain.SessionDoc) error {
	if snap.ID == "" || snap.DocHash == "" || doc.Hash == "" {
		return domain.ErrIntegrity
	}
	if snap.ID != snap.DocHash || snap.DocHash != doc.Hash {
		return domain.ErrIntegrity
	}
	return validateSessionDocHash(doc)
}

func equalHashList(left, right []domain.ContentHash) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

// validateSnapshotGraph verifies S3/S4 ownership in the pre-save virtual graph (existing + incoming). New graft edges can only use appendDiverged, and clients are allowed to echo the received overlay from the server onto the existing snapshot.
func validateSnapshotGraph(existing, incoming []domain.Snapshot) error {
	graph := make(map[domain.ContentHash]domain.Snapshot, len(existing)+len(incoming))
	for _, snap := range existing {
		graph[snap.ID] = snap
	}
	incomingIDs := make(map[domain.ContentHash]bool, len(incoming))
	for _, snap := range incoming {
		if incomingIDs[snap.ID] {
			return fmt.Errorf("%w: duplicate snapshot %s in commit batch", domain.ErrIntegrity, snap.ID)
		}
		incomingIDs[snap.ID] = true
		if current, ok := graph[snap.ID]; ok {
			if !equalHashList(current.Parents, snap.Parents) {
				return fmt.Errorf("%w: snapshot %s parent metadata disagrees with stored object", domain.ErrIntegrity, snap.ID)
			}
			allowedGrafts := make(map[domain.ContentHash]bool, len(current.GraftParents))
			for _, parent := range current.GraftParents {
				allowedGrafts[parent] = true
			}
			for _, parent := range snap.GraftParents {
				if !allowedGrafts[parent] {
					return fmt.Errorf("%w: client cannot add graft parent to snapshot %s", domain.ErrIntegrity, snap.ID)
				}
			}
			if snap.Grafted && !current.Grafted {
				return fmt.Errorf("%w: client cannot mark snapshot %s as grafted", domain.ErrIntegrity, snap.ID)
			}
			if snap.GraftSeq > current.GraftSeq {
				return fmt.Errorf("%w: client cannot advance graft sequence for snapshot %s", domain.ErrIntegrity, snap.ID)
			}
			continue
		}
		if snap.Grafted || len(snap.GraftParents) > 0 || snap.GraftSeq != 0 {
			return fmt.Errorf("%w: new snapshot %s contains server-owned graft metadata", domain.ErrIntegrity, snap.ID)
		}
		graph[snap.ID] = snap
	}

	state := make(map[domain.ContentHash]uint8, len(graph))
	var visit func(domain.ContentHash) error
	visit = func(id domain.ContentHash) error {
		switch state[id] {
		case 1:
			return fmt.Errorf("%w: snapshot graph contains a cycle at %s", domain.ErrIntegrity, id)
		case 2:
			return nil
		}
		snap, ok := graph[id]
		if !ok {
			return fmt.Errorf("%w: missing snapshot parent %s", domain.ErrIntegrity, id)
		}
		state[id] = 1
		seenParents := map[domain.ContentHash]bool{}
		for _, parent := range snap.ReachabilityParents() {
			if parent == id {
				return fmt.Errorf("%w: snapshot %s cannot parent itself", domain.ErrIntegrity, id)
			}
			if seenParents[parent] {
				return fmt.Errorf("%w: snapshot %s contains duplicate parent %s", domain.ErrIntegrity, id, parent)
			}
			seenParents[parent] = true
			if _, ok := graph[parent]; !ok {
				return fmt.Errorf("%w: snapshot %s references missing parent %s", domain.ErrIntegrity, id, parent)
			}
			if err := visit(parent); err != nil {
				return err
			}
		}
		state[id] = 2
		return nil
	}
	for id := range incomingIDs {
		if err := visit(id); err != nil {
			return err
		}
	}
	return nil
}

func settingsObjectHash(bundle domain.SettingsBundle) (domain.ContentHash, error) {
	return domain.SettingsObjectHash(bundle)
}

// --- push/pull use-cases ---

// Negotiate: want (server missing parts) = client haves \ server has (sync protocol step A).
func (s *Service) Negotiate(ctx context.Context, in inbound.PushNegotiateInput) (inbound.PushNegotiateOutput, error) {
	if err := domain.ValidateContentHash(in.RepoID); err != nil {
		return inbound.PushNegotiateOutput{}, err
	}
	if err := validateHashes(in.SnapshotHaves...); err != nil {
		return inbound.PushNegotiateOutput{}, err
	}
	if err := validateHashes(in.DocHaves...); err != nil {
		return inbound.PushNegotiateOutput{}, err
	}
	haveSnaps, err := s.meta.HasSnapshots(ctx, in.RepoID, in.SnapshotHaves)
	if err != nil {
		return inbound.PushNegotiateOutput{}, err
	}
	haveDocs, err := s.blobs.HasDocs(ctx, in.RepoID, in.DocHaves)
	if err != nil {
		return inbound.PushNegotiateOutput{}, err
	}
	if err := validateHashes(in.ChunkHaves...); err != nil {
		return inbound.PushNegotiateOutput{}, err
	}
	var chunkWants []domain.ContentHash
	if len(in.ChunkHaves) > 0 {
		haveChunks, cerr := s.blobs.HasChunks(ctx, in.RepoID, in.ChunkHaves)
		if cerr != nil {
			return inbound.PushNegotiateOutput{}, cerr
		}
		chunkWants = difference(in.ChunkHaves, haveChunks)
	}
	return inbound.PushNegotiateOutput{
		SnapshotWants:          difference(in.SnapshotHaves, haveSnaps),
		DocWants:               difference(in.DocHaves, haveDocs),
		ChunksSupported:        true,
		BoundedChunksSupported: true,
		ChunkFormatsSupported:  []string{domain.ChunkFormatV1, domain.ChunkFormatV2},
		CIRVersionsSupported:   domain.SupportedCIRVersions(),
		ChunkWants:             chunkWants,
	}, nil
}

// StoreChunks is the content-addressed staging phase before a complete doc commit. It limits the sum of each request's raw content, ensuring it stays within the operational proxy body limit even with JSON base64 overhead.
func (s *Service) StoreChunks(ctx context.Context, in inbound.StoreChunksInput) (inbound.StoreChunksOutput, error) {
	if err := domain.ValidateContentHash(in.RepoID); err != nil {
		return inbound.StoreChunksOutput{}, err
	}
	if len(in.Chunks) == 0 || len(in.Chunks) > inbound.MaxChunkWireObjects {
		return inbound.StoreChunksOutput{}, fmt.Errorf("%w: chunk batch count must be 1..%d", domain.ErrValidation, inbound.MaxChunkWireObjects)
	}
	repo, err := s.meta.GetRepo(ctx, in.RepoID)
	if err != nil {
		return inbound.StoreChunksOutput{}, err
	}
	if repo.WorkspaceID == "" {
		return inbound.StoreChunksOutput{}, fmt.Errorf("%w: repo is not bound to a workspace", domain.ErrForbidden)
	}
	bodies := make(map[domain.ContentHash][]byte, len(in.Chunks))
	total := 0
	for _, chunk := range in.Chunks {
		if err := domain.ValidateContentHash(chunk.Hash); err != nil {
			return inbound.StoreChunksOutput{}, err
		}
		if len(chunk.Data) == 0 || len(chunk.Data) > inbound.MaxChunkWireRawBytes {
			return inbound.StoreChunksOutput{}, fmt.Errorf("%w: chunk %s exceeds bounded transport size", domain.ErrValidation, chunk.Hash)
		}
		if domain.HashContent(chunk.Data) != chunk.Hash {
			return inbound.StoreChunksOutput{}, fmt.Errorf("%w: chunk %s body hash mismatch", domain.ErrIntegrity, chunk.Hash)
		}
		if previous, exists := bodies[chunk.Hash]; exists {
			if !bytes.Equal(previous, chunk.Data) {
				return inbound.StoreChunksOutput{}, fmt.Errorf("%w: duplicate chunk %s disagrees", domain.ErrIntegrity, chunk.Hash)
			}
			continue
		}
		total += len(chunk.Data)
		if total > inbound.MaxChunkWireRawBytes {
			return inbound.StoreChunksOutput{}, fmt.Errorf("%w: chunk batch exceeds bounded transport size", domain.ErrValidation)
		}
		bodies[chunk.Hash] = chunk.Data
	}
	stored, deduped, err := s.blobs.PutChunks(ctx, in.RepoID, bodies)
	return inbound.StoreChunksOutput{Stored: stored, Deduped: deduped}, err
}

// PullChunks returns the request prefix up to the raw upper bound. If the first chunk exceeds the upper bound, it stops with an explicit validation error to preserve the cause instead of the proxy's ambiguous 413.
func (s *Service) PullChunks(ctx context.Context, in inbound.PullChunksInput) (inbound.PullChunksOutput, error) {
	if err := domain.ValidateContentHash(in.RepoID); err != nil {
		return inbound.PullChunksOutput{}, err
	}
	if len(in.Wants) == 0 || len(in.Wants) > inbound.MaxChunkWireObjects {
		return inbound.PullChunksOutput{}, fmt.Errorf("%w: chunk want count must be 1..%d", domain.ErrValidation, inbound.MaxChunkWireObjects)
	}
	if err := validateHashes(in.Wants...); err != nil {
		return inbound.PullChunksOutput{}, err
	}
	seen := make(map[domain.ContentHash]bool, len(in.Wants))
	total := 0
	var out inbound.PullChunksOutput
	for _, hash := range in.Wants {
		if seen[hash] {
			return inbound.PullChunksOutput{}, fmt.Errorf("%w: duplicate chunk want %s", domain.ErrValidation, hash)
		}
		seen[hash] = true
		body, err := s.blobs.GetChunk(ctx, in.RepoID, hash)
		if err != nil {
			return inbound.PullChunksOutput{}, err
		}
		if len(body) == 0 || len(body) > inbound.MaxChunkWireRawBytes {
			return inbound.PullChunksOutput{}, fmt.Errorf("%w: chunk %s exceeds bounded transport size", domain.ErrValidation, hash)
		}
		if len(out.ChunkObjects) > 0 && total+len(body) > inbound.MaxChunkWireRawBytes {
			break
		}
		total += len(body)
		out.ChunkObjects = append(out.ChunkObjects, inbound.ChunkObject{Hash: hash, Data: body})
	}
	return out, nil
}

// Commit: stores docs in the order of blobs → snapshots (W1). Full integrity verification followed by content-addressed deduplication.
func (s *Service) Commit(ctx context.Context, in inbound.CommitInput) (inbound.CommitOutput, error) {
	if err := domain.ValidateContentHash(in.RepoID); err != nil {
		return inbound.CommitOutput{}, err
	}
	// Reassemble a chunked wire document from chunks received in this request plus repository-owned stored chunks. Verify the canonical document hash, then pass the complete document through the ordinary validation and storage path.
	docs := in.Docs
	if len(in.ChunkedDocs) > 0 {
		bodies := make(map[domain.ContentHash][]byte, len(in.ChunkObjects))
		for _, co := range in.ChunkObjects {
			if err := domain.ValidateContentHash(co.Hash); err != nil {
				return inbound.CommitOutput{}, err
			}
			if domain.HashContent(co.Data) != co.Hash {
				return inbound.CommitOutput{}, fmt.Errorf("%w: chunk %s body hash mismatch", domain.ErrIntegrity, co.Hash)
			}
			bodies[co.Hash] = co.Data
		}
		for _, cd := range in.ChunkedDocs {
			if err := domain.ValidateContentHash(cd.Hash); err != nil {
				return inbound.CommitOutput{}, err
			}
			format := cd.Format
			if format == "" {
				format = domain.ChunkFormatV1
			}
			if !domain.SupportedChunkFormat(format) {
				return inbound.CommitOutput{}, fmt.Errorf("%w: chunked doc %s uses unsupported format %q", domain.ErrValidation, cd.Hash, cd.Format)
			}
			chunks := make([][]byte, 0, len(cd.Chunks))
			for _, ch := range cd.Chunks {
				if err := domain.ValidateContentHash(ch); err != nil {
					return inbound.CommitOutput{}, err
				}
				b, ok := bodies[ch]
				if !ok {
					var gerr error
					if b, gerr = s.blobs.GetChunk(ctx, in.RepoID, ch); gerr != nil {
						return inbound.CommitOutput{}, fmt.Errorf("%w: chunked doc %s references unknown chunk %s", domain.ErrIntegrity, cd.Hash, ch)
					}
				}
				chunks = append(chunks, b)
			}
			cb, aerr := domain.AssembleDocChunks(domain.DocChunkManifest{Format: format, Envelope: cd.Envelope, Chunks: cd.Chunks}, chunks, cd.Hash)
			if aerr != nil {
				return inbound.CommitOutput{}, fmt.Errorf("%w: chunked doc %s reassembly mismatch", domain.ErrIntegrity, cd.Hash)
			}
			var cir domain.CIRDocument
			if err := json.Unmarshal(cb, &cir); err != nil {
				return inbound.CommitOutput{}, fmt.Errorf("%w: chunked doc %s undecodable", domain.ErrIntegrity, cd.Hash)
			}
			docs = append(docs, domain.SessionDoc{Hash: cd.Hash, CIR: cir})
		}
	}
	docByHash := make(map[domain.ContentHash]domain.SessionDoc, len(docs))
	for _, d := range docs {
		if err := validateSessionDocHash(d); err != nil {
			return inbound.CommitOutput{}, err
		}
		docByHash[d.Hash] = d
	}
	verifiedSettings := make(map[domain.ContentHash]bool)
	normalizedSnaps := make([]domain.Snapshot, 0, len(in.Snapshots))
	for _, snap := range in.Snapshots {
		snap.RepoID = in.RepoID
		// Branches is a response-specific projection calculated from the branch reflog. If the client stores the sent value, it can introduce fake membership in the next response through the FS adapter.
		snap.Branches = nil
		if err := validateSnapshotHashes(snap); err != nil {
			return inbound.CommitOutput{}, err
		}
		if d, ok := docByHash[snap.DocHash]; ok {
			if err := validateSnapshotDocPair(snap, d); err != nil {
				return inbound.CommitOutput{}, err
			}
		} else {
			existing, err := s.blobs.GetDoc(ctx, in.RepoID, snap.DocHash)
			if err != nil {
				if errors.Is(err, domain.ErrNotFound) {
					return inbound.CommitOutput{}, fmt.Errorf("%w: snapshot %s references an absent doc %s (send doc first via push/objects)", domain.ErrIntegrity, snap.ID, snap.DocHash)
				}
				return inbound.CommitOutput{}, err
			}
			if err := validateSnapshotDocPair(snap, existing); err != nil {
				return inbound.CommitOutput{}, err
			}
		}
		for _, hash := range []domain.ContentHash{snap.ClaudeSettings, snap.AgentsSettings, snap.CodexSettings} {
			if hash == "" || verifiedSettings[hash] {
				continue
			}
			if _, err := s.meta.GetSettingsObject(ctx, in.RepoID, hash); err != nil {
				if errors.Is(err, domain.ErrNotFound) {
					return inbound.CommitOutput{}, fmt.Errorf("%w: snapshot %s references missing settings object %s", domain.ErrIntegrity, snap.ID, hash)
				}
				return inbound.CommitOutput{}, err
			}
			verifiedSettings[hash] = true
		}
		// MemoryHash is derived metadata. Only PutMemoryDigest may attach it after
		// validating the digest hash, snapshot ownership, and repo-scoped blob grant.
		snap.MemoryHash = ""
		normalizedSnaps = append(normalizedSnaps, snap)
	}
	repo, err := s.meta.GetRepo(ctx, in.RepoID)
	if err != nil {
		return inbound.CommitOutput{}, err
	}
	if repo.WorkspaceID == "" {
		return inbound.CommitOutput{}, fmt.Errorf("%w: repo is not bound to a workspace", domain.ErrForbidden)
	}
	existingSnaps, err := s.meta.ListSnapshots(ctx, in.RepoID, "")
	if err != nil {
		return inbound.CommitOutput{}, err
	}
	if err := validateSnapshotGraph(existingSnaps, normalizedSnaps); err != nil {
		return inbound.CommitOutput{}, err
	}
	var out inbound.CommitOutput
	for _, d := range docs {
		stored, err := s.blobs.PutDoc(ctx, in.RepoID, d)
		if err != nil {
			return inbound.CommitOutput{}, err
		}
		if stored {
			out.StoredDocs++
		} else {
			out.DedupedDocs++
		}
	}
	for _, snap := range normalizedSnaps {
		if _, err := s.meta.GetSnapshot(ctx, in.RepoID, snap.ID); err == nil {
			out.DedupedSnapshots++
		} else {
			out.StoredSnapshots++
		}
		if err := s.meta.PutSnapshot(ctx, snap); err != nil {
			return inbound.CommitOutput{}, err
		}
	}
	return out, nil
}

// UpdateRef: Update the reference based on the current target and CAS (sync protocol).
//
// Similar to git policy: Branches are allowed to move only in a fast-forward manner (fast-forward), and
// any other move (non-fast-forward/diverged) is rejected with ErrNonFastForward — except for Force.
// Tags are immutable: Moving to another target is rejected without Force.
func (s *Service) UpdateRef(ctx context.Context, in inbound.UpdateRefInput) (inbound.UpdateRefOutput, error) {
	out, err := s.updateRef(ctx, in)
	if err == nil {
		_, lifecycle, _ := domain.ParseBranchLifecycleRef(in.Ref)
		if in.Ref.Kind == domain.RefBranch || lifecycle {
			s.reconcileSharedPendingPointers(ctx, in.RepoID)
		}
	}
	return out, err
}

// UpdateRefs is the request-level ref transaction boundary. Ref projections
// remain individually CAS-checked, but pending reachability is reconciled once
// after the complete batch. An empty retry is intentional: it repairs a server
// crash that occurred after the last ref write but before metadata cleanup.
func (s *Service) UpdateRefs(ctx context.Context, in inbound.UpdateRefsInput) (out inbound.UpdateRefsOutput, err error) {
	if err := domain.ValidateContentHash(in.RepoID); err != nil {
		return out, err
	}
	if len(in.Updates) > inbound.MaxRefBatchUpdates {
		return out, fmt.Errorf("%w: at most %d ref updates per batch", domain.ErrValidation, inbound.MaxRefBatchUpdates)
	}
	defer s.reconcileSharedPendingPointers(ctx, in.RepoID)
	var rejected []string
	for _, update := range in.Updates {
		update.RepoID = in.RepoID
		if _, updateErr := s.updateRef(ctx, update); updateErr != nil {
			if errors.Is(updateErr, domain.ErrNonFastForward) {
				rejected = append(rejected, string(update.Ref.Kind)+"/"+update.Ref.Name)
				continue
			}
			return out, updateErr
		}
		out.Applied++
	}
	if len(rejected) > 0 {
		return out, fmt.Errorf("%w: %s", domain.ErrNonFastForward, strings.Join(rejected, ", "))
	}
	return out, nil
}

func (s *Service) updateRef(ctx context.Context, in inbound.UpdateRefInput) (inbound.UpdateRefOutput, error) {
	if err := domain.ValidateContentHash(in.RepoID); err != nil {
		return inbound.UpdateRefOutput{}, err
	}
	in.Ref.RepoID = in.RepoID
	if err := domain.ValidateRef(in.Ref); err != nil {
		return inbound.UpdateRefOutput{}, err
	}
	if in.Ref.Kind == domain.RefHead && in.Ref.Symbolic != "" {
		branch := strings.TrimPrefix(in.Ref.Symbolic, "refs/heads/")
		if _, err := s.meta.GetRef(ctx, in.RepoID, domain.RefBranch, branch); err != nil {
			return inbound.UpdateRefOutput{}, fmt.Errorf("%w: symbolic HEAD branch %q does not exist", domain.ErrIntegrity, branch)
		}
	} else if _, err := s.meta.GetSnapshot(ctx, in.RepoID, in.Ref.Target); err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return inbound.UpdateRefOutput{}, fmt.Errorf("%w: ref target snapshot %s does not exist", domain.ErrIntegrity, in.Ref.Target)
		}
		return inbound.UpdateRefOutput{}, err
	}
	if event, lifecycle, err := domain.ParseBranchLifecycleRef(in.Ref); err != nil {
		return inbound.UpdateRefOutput{}, err
	} else if lifecycle {
		if in.Force || in.Append {
			return inbound.UpdateRefOutput{}, fmt.Errorf("%w: branch lifecycle events are immutable", domain.ErrValidation)
		}
		if event.State == domain.BranchArchived {
			if repo, repoErr := s.meta.GetRepo(ctx, in.RepoID); repoErr == nil &&
				repo.ProtectDefault && event.Branch == repo.DefaultBranch {
				return inbound.UpdateRefOutput{}, fmt.Errorf("%w: archiving protected branch %q is forbidden", domain.ErrForbidden, event.Branch)
			} else if repoErr != nil && !errors.Is(repoErr, domain.ErrNotFound) {
				return inbound.UpdateRefOutput{}, repoErr
			}
		}
		_, currentErr := s.meta.GetRef(ctx, in.RepoID, domain.RefTag, in.Ref.Name)
		if currentErr != nil && !errors.Is(currentErr, domain.ErrNotFound) {
			return inbound.UpdateRefOutput{}, currentErr
		}
		if err := s.meta.ApplyBranchLifecycleRef(ctx, in.RepoID, in.Ref); err != nil {
			return inbound.UpdateRefOutput{}, err
		}
		result := inbound.RefFastForward
		if currentErr == nil {
			result = inbound.RefUpToDate
		}
		return inbound.UpdateRefOutput{
			Ref: in.Ref, RequestedTarget: in.Ref.Target, ServerTarget: in.Ref.Target, Result: result,
		}, nil
	}
	cur, err := s.meta.GetRef(ctx, in.RepoID, in.Ref.Kind, in.Ref.Name)
	serverTarget := domain.ContentHash("")
	if err == nil {
		serverTarget = cur.Target
	} else if !errors.Is(err, domain.ErrNotFound) {
		return inbound.UpdateRefOutput{}, err
	}
	out := inbound.UpdateRefOutput{Ref: in.Ref, RequestedTarget: in.Ref.Target, ServerTarget: serverTarget}

	if in.Ref.Kind == domain.RefHead {
		if err := s.meta.CompareAndSwapRef(ctx, in.RepoID, in.Ref, serverTarget); err != nil {
			return inbound.UpdateRefOutput{}, err
		}
		out.Result = inbound.RefFastForward
		out.ServerTarget = in.Ref.Target
		return out, nil
	}

	// session ref is a server internal pointer proving the git-branch scope left by a partial join. If created or moved in the general ref API, the client can spoof the scope to bypass cross-branch join restrictions. Only the same pointer echo after pull followed by push is allowed.
	if in.Ref.Kind == domain.RefSession {
		if err == nil && serverTarget == in.Ref.Target {
			out.Result = inbound.RefUpToDate
			return out, nil
		}
		return inbound.UpdateRefOutput{}, fmt.Errorf("%w: session refs are managed by join", domain.ErrForbidden)
	}

	// --force: move without ancestry classification, while still using CAS against the observed current value.
	if in.Force {
		// Protected branch: Basic branch rejects --force moves (record is P1 immutable —
		// this is a separate policy to block pointer force moves, akin to GitHub's protected branch handling).
		if in.Ref.Kind == domain.RefBranch {
			if repo, rerr := s.meta.GetRepo(ctx, in.RepoID); rerr == nil &&
				repo.ProtectDefault && in.Ref.Name == repo.DefaultBranch {
				return inbound.UpdateRefOutput{}, fmt.Errorf("%w: --force move is forbidden on protected branch %q", domain.ErrForbidden, in.Ref.Name)
			}
		}
		if serverTarget == in.Ref.Target {
			out.Result = inbound.RefUpToDate
			return out, nil
		}
		if err := s.meta.CompareAndSwapRef(ctx, in.RepoID, in.Ref, serverTarget); err != nil {
			return inbound.UpdateRefOutput{}, err
		}
		out.Result = inbound.RefForced
		out.ServerTarget = in.Ref.Target
		s.notifyRefUpdate(ctx, in.RepoID, in.Ref, true, serverTarget == "")
		return out, nil
	}

	// Tag immutability rule (same as git): Reject if it already exists and the target is different.
	if in.Ref.Kind == domain.RefTag {
		switch serverTarget {
		case "":
			if err := s.meta.CompareAndSwapRef(ctx, in.RepoID, in.Ref, ""); err != nil {
				return inbound.UpdateRefOutput{}, err
			}
			out.Result = inbound.RefFastForward
			out.ServerTarget = in.Ref.Target
			return out, nil
		case in.Ref.Target:
			out.Result = inbound.RefUpToDate
			return out, nil
		default:
			return inbound.UpdateRefOutput{}, fmt.Errorf("%w: tag %q already points to another snapshot (--force required)", domain.ErrNonFastForward, in.Ref.Name)
		}
	}

	class, err := s.engine.ClassifyRefMove(ctx, in.RepoID, serverTarget, in.Ref.Target)
	if err != nil {
		return inbound.UpdateRefOutput{}, err
	}
	switch class {
	case outbound.MoveUpToDate:
		out.Result = inbound.RefUpToDate
		return out, nil
	case outbound.MoveFastForward:
		if err := s.meta.CompareAndSwapRef(ctx, in.RepoID, in.Ref, serverTarget); err != nil {
			return inbound.UpdateRefOutput{}, err
		}
		out.Result = inbound.RefFastForward
		out.ServerTarget = in.Ref.Target
		s.notifyRefUpdate(ctx, in.RepoID, in.Ref, false, serverTarget == "")
		return out, nil
	case outbound.MoveDiverged:
		if in.Append {
			return s.appendDiverged(ctx, in, serverTarget, out)
		}
		return inbound.UpdateRefOutput{}, fmt.Errorf("%w: %s (must include previous commit — use --append to rebase head without loss, or retry after pull)", domain.ErrNonFastForward, in.Ref.Name)
	default: // MoveNonFastForward (behind) → like git, reject (prohibited overwrite)
		return inbound.UpdateRefOutput{}, fmt.Errorf("%w: %s (must include previous commit — retry after pull or use --force)", domain.ErrNonFastForward, in.Ref.Name)
	}
}

// appendDiverged appends a diverged push as an unaltered overlay to the current head.
//
// For incoming chains where the server head is unaware of a segment (segment = ancestry(target) − ancestry(head)), the boundary snapshot's GraftParents are updated to include the current head. All boundary segments, including root parents and shared histories, follow the same rules, and natural parents are never changed.
//
// Safety justification: The segment is disjoint from the head ancestor set and is acyclic. Both existing natural parents and the head are reachable, so reachability is preserved and extended. Replicas with the same ID continue to agree on natural parents.
// Unlike Force, there is no loss, making it non-target of protected branch policies.
// Unlike code, context does not force convergence ("pull is user's responsibility"), so push must always succeed on any divergence — the server DB continues to be updated.
func (s *Service) appendDiverged(ctx context.Context, in inbound.UpdateRefInput, serverTarget domain.ContentHash, out inbound.UpdateRefOutput) (inbound.UpdateRefOutput, error) {
	serverAnc, err := s.engine.AncestorsClosure(ctx, in.RepoID, []domain.ContentHash{serverTarget})
	if err != nil {
		return inbound.UpdateRefOutput{}, err
	}
	shared := make(map[domain.ContentHash]bool, len(serverAnc))
	for _, h := range serverAnc {
		shared[h] = true
	}
	targetAnc, err := s.engine.AncestorsClosure(ctx, in.RepoID, []domain.ContentHash{in.Ref.Target})
	if err != nil {
		return inbound.UpdateRefOutput{}, err
	}
	var segment []domain.ContentHash
	for _, h := range targetAnc {
		if !shared[h] {
			segment = append(segment, h)
		}
	}
	sort.Slice(segment, func(i, j int) bool { return segment[i] < segment[j] }) // deterministic processing order
	for _, id := range segment {
		snap, gerr := s.meta.GetSnapshot(ctx, in.RepoID, id)
		if gerr != nil {
			return inbound.UpdateRefOutput{}, fmt.Errorf("%w: missing snapshot %s in append target lineage", domain.ErrValidation, id)
		}
		// Overlay graft: Parents (original) are left unchanged, and the shared history boundary (or irrelevant ancestor root) is grafted with serverTarget (head). This allows the old head to be reached, and (parents ∪ graft_parents), while maintaining the same Parents for local/server replicas to prevent replica parent disagreement (permanent divergence removal).
		needsGraft := len(snap.Parents) == 0
		for _, p := range snap.Parents {
			if shared[p] {
				needsGraft = true
				break
			}
		}
		if !needsGraft {
			continue
		}
		if err := s.meta.AddGraftParents(ctx, in.RepoID, id, []domain.ContentHash{serverTarget}); err != nil {
			return inbound.UpdateRefOutput{}, err
		}
	}
	if err := s.meta.CompareAndSwapRef(ctx, in.RepoID, in.Ref, serverTarget); err != nil {
		return inbound.UpdateRefOutput{}, err
	}
	out.Result = inbound.RefAppended
	out.ServerTarget = in.Ref.Target
	s.notifyRefUpdate(ctx, in.RepoID, in.Ref, false, false)
	return out, nil
}

// Send: reply to pull request with missing objects (snapshot meta + doc) (read-only).
func (s *Service) Send(ctx context.Context, in inbound.PullSendInput) (inbound.PullSendOutput, error) {
	if err := domain.ValidateContentHash(in.RepoID); err != nil {
		return inbound.PullSendOutput{}, err
	}
	if err := validateHashes(in.SnapshotWants...); err != nil {
		return inbound.PullSendOutput{}, err
	}
	if err := validateHashes(in.DocWants...); err != nil {
		return inbound.PullSendOutput{}, err
	}
	out := inbound.PullSendOutput{BoundedChunksSupported: true}
	supportedCIR := requestedCIRVersions(in.CIRVersionsSupported)
	for _, id := range in.SnapshotWants {
		snap, err := s.meta.GetSnapshot(ctx, in.RepoID, id)
		if err != nil {
			return inbound.PullSendOutput{}, err
		}
		out.Snapshots = append(out.Snapshots, snap)
	}
	for _, h := range in.DocWants {
		doc, err := s.blobs.GetDoc(ctx, in.RepoID, h)
		if err != nil {
			return inbound.PullSendOutput{}, err
		}
		if err := requireSupportedCIRVersion(doc.CIR.Envelope.CIRVersion, supportedCIR); err != nil {
			return inbound.PullSendOutput{}, err
		}
		out.Docs = append(out.Docs, doc)
	}
	// Chunk wire (delta download): manifest reply — plan-unavailable doc (ErrNotFound) is a fallback reply (client validates if manifests ∪ docs cover the entire request).
	if err := validateHashes(in.DocManifestWants...); err != nil {
		return inbound.PullSendOutput{}, err
	}
	supportedFormats := requestedChunkFormats(in.ChunkFormatsSupported)
	for _, h := range in.DocManifestWants {
		man, merr := s.blobs.GetDocManifest(ctx, in.RepoID, h)
		if merr != nil {
			if errors.Is(merr, domain.ErrNotFound) {
				doc, derr := s.blobs.GetDoc(ctx, in.RepoID, h)
				if derr != nil {
					return inbound.PullSendOutput{}, derr
				}
				if err := requireSupportedCIRVersion(doc.CIR.Envelope.CIRVersion, supportedCIR); err != nil {
					return inbound.PullSendOutput{}, err
				}
				out.Docs = append(out.Docs, doc)
				continue
			}
			return inbound.PullSendOutput{}, merr
		}
		var envelope struct {
			CIRVersion string `json:"cir_version"`
		}
		if err := json.Unmarshal(man.Envelope, &envelope); err != nil {
			return inbound.PullSendOutput{}, fmt.Errorf("%w: doc %s has an invalid chunk envelope", domain.ErrIntegrity, h)
		}
		if err := requireSupportedCIRVersion(envelope.CIRVersion, supportedCIR); err != nil {
			return inbound.PullSendOutput{}, err
		}
		if supportedFormats[man.Format] {
			out.DocManifests = append(out.DocManifests, inbound.ChunkedDoc{Hash: h, Format: man.Format, Envelope: man.Envelope, Chunks: man.Chunks})
			continue
		}
		// The stored representation is newer/different from the requesting peer.
		// Return the complete document instead of materializing a second chunk
		// format as a side effect of this read-only operation.
		doc, derr := s.blobs.GetDoc(ctx, in.RepoID, h)
		if derr != nil {
			return inbound.PullSendOutput{}, derr
		}
		out.Docs = append(out.Docs, doc)
	}
	if err := validateHashes(in.ChunkWants...); err != nil {
		return inbound.PullSendOutput{}, err
	}
	for _, ch := range in.ChunkWants {
		b, cerr := s.blobs.GetChunk(ctx, in.RepoID, ch)
		if cerr != nil {
			return inbound.PullSendOutput{}, cerr
		}
		out.ChunkObjects = append(out.ChunkObjects, inbound.ChunkObject{Hash: ch, Data: b})
	}
	return out, nil
}

func requestedCIRVersions(versions []string) map[string]bool {
	if len(versions) == 0 {
		return map[string]bool{domain.CIRVersionV1: true}
	}
	out := make(map[string]bool, len(versions))
	for _, version := range versions {
		if domain.SupportsCIRVersion(domain.SupportedCIRVersions(), version) {
			out[version] = true
		}
	}
	return out
}

func requireSupportedCIRVersion(version string, supported map[string]bool) error {
	if version == "" {
		version = domain.CIRVersionV1
	}
	if supported[version] {
		return nil
	}
	return fmt.Errorf("%w: peer does not advertise CIR %s support", domain.ErrUnsupportedCIRVersion, version)
}

func requestedChunkFormats(formats []string) map[string]bool {
	if len(formats) == 0 {
		return map[string]bool{domain.ChunkFormatV1: true}
	}
	out := make(map[string]bool, len(formats))
	for _, format := range formats {
		if domain.SupportedChunkFormat(format) {
			if format == "" {
				format = domain.ChunkFormatV1
			}
			out[format] = true
		}
	}
	return out
}

// PromoteSnapshotMessage: hook label → commit message one-way promotion (idempotent).
// The only exception to immutable snapshot meta message — any rule-breaking rewrite is rejected:
// If the stored message does not have the hook prefix, only the same message (idempotent retry) is allowed.
func (s *Service) PromoteSnapshotMessage(ctx context.Context, repoID, id domain.ContentHash, message string) error {
	if err := domain.ValidateContentHash(repoID); err != nil {
		return err
	}
	if err := domain.ValidateContentHash(id); err != nil {
		return err
	}
	message = strings.TrimSpace(message)
	// Length is based on character count (same unit as openapi maxLength) — bytes are truncated much shorter than document limit (review P3).
	if message == "" || utf8.RuneCountInString(message) > 2000 || strings.HasPrefix(message, domain.HookMessagePrefix) {
		return fmt.Errorf("%w: invalid promotion message", domain.ErrValidation)
	}
	// The storage-layer CAS (UpdateSnapshotMessage) makes the final one-way-rule decision;
	// reading and deciding here would introduce a check-then-act race. State conflicts make promotion impossible, so
	// ErrConflict (409, matching openapi contract) — not an integrity violation (422).
	return s.meta.UpdateSnapshotMessage(ctx, repoID, id, message)
}

// GraftSnapshotParents adds reachability overlay edges to snapshots (Parents immutable, idempotent).
// Path to propagate reachability preservation of client sibling advances (multi-session commits) to server replicas —
// inventory-only push does not resend metadata updates of existing objects. Each graft parent must be a real snapshot in the repo (to prevent reachability pollution with arbitrary hashes).
func (s *Service) GraftSnapshotParents(ctx context.Context, repoID, id domain.ContentHash, parents []domain.ContentHash, expectedSeq uint64) error {
	if err := domain.ValidateContentHash(repoID); err != nil {
		return err
	}
	if err := domain.ValidateContentHash(id); err != nil {
		return err
	}
	if len(parents) == 0 || len(parents) > 16 {
		return fmt.Errorf("%w: graft parents must be 1..16", domain.ErrValidation)
	}
	if _, err := s.meta.GetSnapshot(ctx, repoID, id); err != nil {
		return err
	}
	for _, p := range parents {
		if err := domain.ValidateContentHash(p); err != nil {
			return err
		}
		if p == id {
			return fmt.Errorf("%w: self graft", domain.ErrValidation)
		}
		if _, err := s.meta.GetSnapshot(ctx, repoID, p); err != nil {
			return fmt.Errorf("%w: graft parent %s not found", domain.ErrNotFound, p)
		}
	}
	return s.meta.AddGraftParentsCAS(ctx, repoID, id, parents, expectedSeq)
}

// List: branch-specific snapshot metadata list.
func (s *Service) List(ctx context.Context, in inbound.ListSnapshotsInput) ([]domain.Snapshot, error) {
	if err := domain.ValidateContentHash(in.RepoID); err != nil {
		return nil, err
	}
	snaps, err := s.meta.ListSnapshots(ctx, in.RepoID, "")
	if err != nil {
		return nil, err
	}
	members, err := s.snapshotBranchMemberships(ctx, in.RepoID, snaps)
	if err != nil {
		return nil, err
	}
	out := make([]domain.Snapshot, 0, len(snaps))
	for _, snap := range snaps {
		snap.Branches = nil // Removes leftover projections from old FS versions before response assembly.
		for branch := range members[snap.ID] {
			snap.Branches = append(snap.Branches, branch)
		}
		sort.Strings(snap.Branches)
		if in.Branch == "" || members[snap.ID][in.Branch] {
			out = append(out, snap)
		}
	}
	return out, nil
}

// snapshotBranchMemberships complements hash-outside scalar Branch's first-writer limitation with branch reflog. A snapshot belongs to a git branch if it was the old/new target of that branch ref at some point. Legacy before reflog uses Snapshot.Branch as a seed.
func (s *Service) snapshotBranchMemberships(ctx context.Context, repoID domain.ContentHash, snaps []domain.Snapshot) (map[domain.ContentHash]map[string]bool, error) {
	members := make(map[domain.ContentHash]map[string]bool, len(snaps))
	add := func(id domain.ContentHash, branch string) {
		if id == "" || branch == "" || branch == "HEAD" {
			return
		}
		if members[id] == nil {
			members[id] = map[string]bool{}
		}
		members[id][branch] = true
	}
	for _, snap := range snaps {
		add(snap.ID, snap.Branch)
	}
	logs, err := s.meta.ReadReflog(ctx, repoID)
	if err != nil {
		return nil, err
	}
	for _, entry := range logs {
		if entry.Kind != domain.RefBranch {
			continue
		}
		add(entry.Old, entry.Name)
		add(entry.New, entry.Name)
	}
	return members, nil
}

// Fsck: reference reachability audit (read-only, git fsck equivalent). Creates a reachability set by following parents from all refs and reports dangling snapshots, missing parent references (corruption), and parentless roots. Does not fix anything — parentless roots are treated as normal roots (not artificially attaching parents).
func (s *Service) Fsck(ctx context.Context, repoID domain.ContentHash) (inbound.FsckReport, error) {
	if err := domain.ValidateContentHash(repoID); err != nil {
		return inbound.FsckReport{}, err
	}
	snaps, err := s.meta.ListSnapshots(ctx, repoID, "")
	if err != nil {
		return inbound.FsckReport{}, err
	}
	byID := make(map[domain.ContentHash]bool, len(snaps))
	for _, sn := range snaps {
		byID[sn.ID] = true
	}
	refs, err := s.meta.ListRefs(ctx, repoID)
	if err != nil {
		return inbound.FsckReport{}, err
	}
	// Follow parents from ref target using BFS → Reachability set (only existing objects).
	reachable := map[domain.ContentHash]bool{}
	var stack []domain.ContentHash
	for _, r := range refs {
		if r.Target != "" {
			stack = append(stack, r.Target)
		}
	}
	// Pending pointers are first-class roots outside the committed branch DAG.
	// Treating active/uncommitted sessions as unreachable makes healthy data look
	// corrupt and obscures real orphan regressions. Dismissed remains a UI-only
	// state; its data is intentionally preserved and therefore still reachable.
	pendings, err := s.meta.ListPendings(ctx, repoID)
	if err != nil {
		return inbound.FsckReport{}, err
	}
	for _, pending := range pendings {
		if pending.Target != "" {
			stack = append(stack, pending.Target)
		}
	}
	// Reachability rule single source: domain.Snapshot.ReachabilityParents(Parents ∪ GraftParents).
	parentsOf := make(map[domain.ContentHash][]domain.ContentHash, len(snaps))
	for _, sn := range snaps {
		parentsOf[sn.ID] = sn.ReachabilityParents()
	}
	for len(stack) > 0 {
		n := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if reachable[n] || !byID[n] {
			continue
		}
		reachable[n] = true
		stack = append(stack, parentsOf[n]...)
	}
	rep := inbound.FsckReport{Total: len(snaps), Reachable: len(reachable)}
	for _, sn := range snaps {
		if len(sn.Parents) == 0 {
			rep.Roots = append(rep.Roots, sn.ID)
		}
		if !reachable[sn.ID] {
			rep.Unreachable = append(rep.Unreachable, sn.ID)
		}
		// Dangling audit follows the same rules as reachability (Parents ∪ GraftParents): A missing snapshot pointer in a graft overlay edge is the exact corruption class that an overlay graft can create.
		for _, p := range sn.ReachabilityParents() {
			if !byID[p] {
				rep.DanglingParents = append(rep.DanglingParents, inbound.DanglingParent{Snapshot: sn.ID, Missing: p})
			}
		}
	}
	sort.Slice(rep.Unreachable, func(i, j int) bool { return rep.Unreachable[i] < rep.Unreachable[j] })
	sort.Slice(rep.Roots, func(i, j int) bool { return rep.Roots[i] < rep.Roots[j] })
	return rep, nil
}

// Reflog: Returns the ref movement history of the repo (latest first) (read-only). Basis for recovering hidden tips.
func (s *Service) Reflog(ctx context.Context, repoID domain.ContentHash) ([]domain.RefLogEntry, error) {
	if err := domain.ValidateContentHash(repoID); err != nil {
		return nil, err
	}
	return s.meta.ReadReflog(ctx, repoID)
}

// Authenticate: Token → Team interpretation (v1: identity enhancement). Invalid tokens result in an error from AuthProvider.
func (s *Service) Authenticate(ctx context.Context, in inbound.AuthInput) (inbound.AuthOutput, error) {
	team, err := s.auth.ResolveTeam(ctx, in.TeamToken)
	if err != nil {
		return inbound.AuthOutput{}, err
	}
	id := in.Identity
	if id.Team == "" {
		id.Team = team
	}
	return inbound.AuthOutput{Team: team, Identity: id}, nil
}

// --- HTTP read pass-through (auxiliary methods for inbound ports) ---

func (s *Service) GetManifest(ctx context.Context, repoID domain.ContentHash) (domain.Manifest, error) {
	if err := domain.ValidateContentHash(repoID); err != nil {
		return domain.Manifest{}, err
	}
	return s.meta.GetManifest(ctx, repoID)
}
func (s *Service) EnsureRepo(ctx context.Context, actorID string, repo domain.Repo) (domain.Repo, error) {
	if err := domain.ValidateContentHash(repo.ID); err != nil {
		return domain.Repo{}, err
	}
	if repo.DefaultBranch != "" {
		if err := domain.ValidateBranchName(repo.DefaultBranch); err != nil {
			return domain.Repo{}, err
		}
	}
	// Binding is always interpreted by the server from the remote URL path (/<owner_username>/<workspace-slug>/…). The workspace_id in the body is untrusted — assuming RepoID from URL hash, allowing client-specified bindings can lead to arbitrary workspace takeover (repo squatting) if a matching workspace is not found (fail-closed).
	repo.RemoteURL = domain.SanitizeRemoteURL(repo.RemoteURL)
	repo.GitRemoteURL = domain.SanitizeRemoteURL(repo.GitRemoteURL)
	wsID, err := s.workspaceForURL(ctx, repo.RemoteURL)
	if err != nil {
		return domain.Repo{}, err
	}
	repo.WorkspaceID = wsID

	// The authenticity of RepoID is not the Git origin but the cxthub destination URL. Connecting the same Git repo to multiple users' workspaces results in different RemoteURLs, thus different RepoIDs.
	expectedID := domain.HashContent([]byte(normalizeGitURL(repo.RemoteURL)))
	if repo.ID != expectedID {
		return domain.Repo{}, fmt.Errorf("%w: repo id does not match cxthub remote URL", domain.ErrIntegrity)
	}

	// Registration itself creates the state of the target workspace. To prevent URL takeover by others before login, context write permissions (member level or above) are required for both initial registration and re-registration.
	role, ok := s.workspaceRole(ctx, wsID, actorID)
	if !ok || !role.AtLeast(domain.RoleMember) {
		return domain.Repo{}, domain.ErrForbidden
	}

	// Git origin verification (onboarding safety measure): If another team member has already connected to the same cxthub URL (same RepoID), and the git origin of the code repo is confirmed, the git origin of the newly connecting local folder must also be the same. If different, it is rejected — "A folder not connected to git in that workspace" is prevented from being attached to the same cxthub URL (same origin: URL is the destination, origin is the substance).
	// The first connector confirms the origin (empty → filled), and thereafter, that value holds authority.
	if existing, err := s.meta.GetRepo(ctx, repo.ID); err == nil && existing.GitRemoteURL != "" {
		if normalizeGitURL(repo.GitRemoteURL) != normalizeGitURL(existing.GitRemoteURL) {
			return domain.Repo{}, fmt.Errorf(
				"%w: The git origin (%s) of this folder is different from the git (%s) connected to this workspace repo — try again in the corresponding code repo",
				domain.ErrGitOriginMismatch, gitOriginLabel(repo.GitRemoteURL), gitOriginLabel(existing.GitRemoteURL))
		}
	}
	return s.meta.PutRepo(ctx, repo)
}

func (s *Service) workspaceRole(ctx context.Context, workspaceID, userID string) (domain.MemberRole, bool) {
	if userID == "" || s.ws == nil {
		return "", false
	}
	wsp, err := s.ws.GetWorkspace(ctx, workspaceID)
	if err != nil {
		return "", false
	}
	if wsp.OwnerID == userID {
		return domain.RoleOwner, true
	}
	members, err := s.ws.ListMembers(ctx, workspaceID)
	if err != nil {
		return "", false
	}
	for _, member := range members {
		if member.UserID == userID && domain.ValidRole(member.Role) {
			return member.Role, true
		}
	}
	return "", false
}

// normalizeGitURL is for normalizing git origin URLs (same rules as CLI gitctx.NormalizeRemoteURL:
// absorbs ssh/scp formats, removes scheme/trailing .git/slash, and lowercases). The backend cannot import the CLI package,
// so this logic is duplicated here — maintaining both ensures validation integrity if rules diverge.
func normalizeGitURL(raw string) string {
	u := domain.SanitizeRemoteURL(raw)
	if u == "" {
		return ""
	}
	if strings.HasPrefix(u, "git@") { // git@github.com:org/repo.git
		u = strings.Replace(strings.TrimPrefix(u, "git@"), ":", "/", 1)
	} else if parsed, err := url.Parse(u); err == nil && parsed.Host != "" {
		u = parsed.Host + parsed.EscapedPath()
	}
	u = strings.TrimSuffix(u, ".git")
	u = strings.TrimRight(u, "/")
	return strings.ToLower(u)
}

// gitOriginLabel is a label for validation failure messages (empty means "(none)").
func gitOriginLabel(raw string) string {
	if strings.TrimSpace(raw) == "" {
		return "(none)"
	}
	return raw
}

// PutMemoryDigest is the rolling-upgrade endpoint for legacy clients. It may
// create an empty attachment or retry the exact current digest, but it cannot
// replace a non-empty pointer because old clients provide no causal parent.
func (s *Service) PutMemoryDigest(ctx context.Context, repoID domain.ContentHash, d domain.MemoryDigest) (domain.ContentHash, error) {
	if d.PreviousMemoryHash != "" {
		return "", fmt.Errorf("%w: causal memory requires the attachment endpoint", domain.ErrValidation)
	}
	return s.putMemoryDigest(ctx, repoID, d, "", false)
}

// PutMemoryDigestCAS advances Snapshot.MemoryHash only from the digest's
// PreviousMemoryHash. The immutable blob is retained even when the pointer CAS
// loses, so neither concurrent memory is destroyed.
func (s *Service) PutMemoryDigestCAS(ctx context.Context, repoID domain.ContentHash, d domain.MemoryDigest) (domain.ContentHash, error) {
	return s.putMemoryDigest(ctx, repoID, d, d.PreviousMemoryHash, true)
}

func (s *Service) putMemoryDigest(ctx context.Context, repoID domain.ContentHash, d domain.MemoryDigest, expected domain.ContentHash, causal bool) (domain.ContentHash, error) {
	if err := validateHashes(repoID, d.SnapshotID); err != nil {
		return "", err
	}
	if err := domain.ValidateOptionalContentHash(d.PreviousMemoryHash); err != nil {
		return "", err
	}
	for _, fragment := range d.Fragments {
		if err := domain.ValidateContentHash(fragment.SourceSnapshot); err != nil {
			return "", err
		}
	}
	if coverage := d.GraftCoverage; coverage != nil {
		if coverage.ProjectionVersion == 0 || coverage.GraftSeq > domain.MaxGraftSeq {
			return "", fmt.Errorf("%w: invalid memory graft coverage version or sequence", domain.ErrIntegrity)
		}
		if coverage.ProjectionComplete && coverage.LineageFingerprint == "" {
			return "", fmt.Errorf("%w: complete memory projection is missing its lineage fingerprint", domain.ErrIntegrity)
		}
		if coverage.LineageFingerprint != "" {
			if err := domain.ValidateContentHash(coverage.LineageFingerprint); err != nil {
				return "", err
			}
		}
		for _, parent := range coverage.GraftParents {
			if err := domain.ValidateContentHash(parent); err != nil {
				return "", err
			}
		}
		for _, source := range coverage.PinnedSources {
			if err := domain.ValidateContentHash(source); err != nil {
				return "", err
			}
		}
	}
	if _, err := s.meta.GetSnapshot(ctx, repoID, d.SnapshotID); err != nil {
		return "", err // ErrNotFound → 404
	}
	if causal && expected != "" {
		previous, err := s.blobs.GetMemory(ctx, repoID, expected)
		if err != nil {
			return "", err
		}
		if previous.SnapshotID != d.SnapshotID {
			return "", fmt.Errorf("%w: memory parent belongs to another snapshot", domain.ErrIntegrity)
		}
	}
	hash, err := s.blobs.PutMemory(ctx, repoID, d)
	if err != nil {
		return "", err
	}
	// Attaches a derivative pointer to the snapshot metadata — after push, the memorize also holds raw+memory (E clause).
	// Without this, pull/web cannot recognize memory existence.
	if err := s.meta.CompareAndSwapSnapshotMemory(ctx, repoID, d.SnapshotID, expected, hash); err != nil {
		return "", err
	}
	return hash, nil
}

// GetMemoryObject reads one immutable attachment object by its own hash. Pull
// uses this to prove ancestry before moving a local memory ref.
func (s *Service) GetMemoryObject(ctx context.Context, repoID, hash domain.ContentHash) (domain.MemoryDigest, error) {
	if err := validateHashes(repoID, hash); err != nil {
		return domain.MemoryDigest{}, err
	}
	return s.blobs.GetMemory(ctx, repoID, hash)
}

// GetMemoryDigest resolves the authoritative MemoryHash pointer. Metadata-only
// fallback is intentionally limited to legacy snapshots with no pointer: once
// a pointer exists, hiding a missing/corrupt blob behind stale metadata would
// turn an integrity failure into a silent rollback.
func (s *Service) GetMemoryDigest(ctx context.Context, repoID, snapshotID domain.ContentHash) (domain.MemoryDigest, error) {
	if err := validateHashes(repoID, snapshotID); err != nil {
		return domain.MemoryDigest{}, err
	}
	snapshot, err := s.meta.GetSnapshot(ctx, repoID, snapshotID)
	if err != nil {
		return domain.MemoryDigest{}, err
	}
	if snapshot.MemoryHash == "" {
		return s.meta.GetMemoryMeta(ctx, repoID, snapshotID)
	}
	digest, err := s.blobs.GetMemory(ctx, repoID, snapshot.MemoryHash)
	if err != nil {
		return domain.MemoryDigest{}, err
	}
	if digest.SnapshotID != snapshotID {
		return domain.MemoryDigest{}, fmt.Errorf("%w: memory %s targets snapshot %s, want %s", domain.ErrIntegrity, snapshot.MemoryHash, digest.SnapshotID, snapshotID)
	}
	return digest, nil
}

// UpdateAbout updates the repo About (web-only — membership check at call site).
func (s *Service) UpdateAbout(ctx context.Context, repoID domain.ContentHash, description, website string, topics []string) error {
	if err := domain.ValidateContentHash(repoID); err != nil {
		return err
	}
	normalizedWebsite, err := normalizeAboutWebsite(website)
	if err != nil {
		return err
	}
	if len(topics) > 20 {
		topics = topics[:20]
	}
	return s.meta.UpdateRepoAbout(ctx, repoID, strings.TrimSpace(description), normalizedWebsite, topics)
}

// UpdateRepoConfig updates the repo structure settings (default branch, protected branch).
func (s *Service) UpdateRepoConfig(ctx context.Context, repoID domain.ContentHash, defaultBranch *string, protectDefault *bool) error {
	if err := domain.ValidateContentHash(repoID); err != nil {
		return err
	}
	if defaultBranch != nil && *defaultBranch != "" {
		if err := domain.ValidateBranchName(*defaultBranch); err != nil {
			return err
		}
	}
	return s.meta.UpdateRepoConfig(ctx, repoID, defaultBranch, protectDefault)
}

// settingsKindOK validates the settings bundle type.
func settingsKindOK(kind string) bool { return domain.ValidSettingsKind(kind) }

// PutSettings stores team default settings bundles. It validates path safety (no relative/parent directory traversal) and
// total size (2MB) — these files are directly unpacked into team members' local .claude/.agents/.codex directories.
func (s *Service) PutSettings(ctx context.Context, repoID domain.ContentHash, bundle domain.SettingsBundle) error {
	if err := domain.ValidateContentHash(repoID); err != nil {
		return err
	}
	if err := domain.ValidateSettingsBundle(bundle.Kind, "", bundle); err != nil {
		return err
	}
	bundle.UpdatedAt = time.Now().UTC()
	return s.meta.PutSettingsBundle(ctx, repoID, bundle)
}

// PutPending upserts the session-specific context pointer (CLI hook capture mirror). The sessionID is authoritative — the body's session/repo is overwritten (push's RepoID normalization equivalent).
// It garbage collects the previous hook capture leaf that was replaced by sliding (prevents graph dangling branches and storage accumulation).
func (s *Service) PutPending(ctx context.Context, repoID domain.ContentHash, sessionID string, p domain.Pending) error {
	if err := domain.ValidateContentHash(repoID); err != nil {
		return err
	}
	if sessionID == "" || len(sessionID) > 128 {
		return fmt.Errorf("%w: session_id is empty or too long", domain.ErrIntegrity)
	}
	p.RepoID = repoID
	p.SessionID = sessionID
	if p.Target == "" {
		return fmt.Errorf("%w: pending.target (snapshot hash) is required", domain.ErrIntegrity)
	}
	if err := domain.ValidateContentHash(p.Target); err != nil {
		return err
	}
	// target object existence validation — pointer/object push is a separate request (non-atomic), so client order (objects-first) does not guarantee server consistency. Accepting a pointer to a non-existent snapshot results in a phantom session in the list but no node in the graph, so we reject it (fail-closed).
	if _, gerr := s.meta.GetSnapshot(ctx, repoID, p.Target); gerr != nil {
		if errors.Is(gerr, domain.ErrNotFound) {
			return fmt.Errorf("%w: pending.target %s snapshot not found on server — object push prerequisite", domain.ErrValidation, p.Target)
		}
		return gerr
	}
	p.UpdatedAt = time.Now().UTC()
	old, err := s.meta.ReplacePending(ctx, repoID, p)
	if err != nil {
		return err
	}
	s.gcHookLeaf(ctx, repoID, old, p.Target)
	return nil
}

// DismissPending marks an uncommitted capture as "hidden in the pending list" (data deletion
// is not performed — snapshot/doc/session history immutable, target preserved, GC protection
// remains). No-op if already dismissed.
func (s *Service) DismissPending(ctx context.Context, repoID domain.ContentHash, sessionID string) error {
	if err := domain.ValidateContentHash(repoID); err != nil {
		return err
	}
	_, err := s.meta.SetPendingDismissed(ctx, repoID, sessionID, true)
	return err
}

// UndismissPending re-adds a hidden pending capture to the list. Only the flag
// changes; the target remains immutable, and subsequent replacements observe
// the cleared flag atomically. No-op if already undismissed.
func (s *Service) UndismissPending(ctx context.Context, repoID domain.ContentHash, sessionID string) error {
	if err := domain.ValidateContentHash(repoID); err != nil {
		return err
	}
	_, err := s.meta.SetPendingDismissed(ctx, repoID, sessionID, false)
	return err
}

// ListPendings returns the durable uncommitted capture pointers for the repo.
func (s *Service) ListPendings(ctx context.Context, repoID domain.ContentHash) ([]domain.Pending, error) {
	if err := domain.ValidateContentHash(repoID); err != nil {
		return nil, err
	}
	return s.meta.ListPendings(ctx, repoID)
}

// DeletePending is the legacy unconditional commit-resolution path. Current
// clients use CompareAndDeletePending so a delayed helper cannot erase a newer
// capture. It is idempotent.
// The hook capture leaf of the released pointer is also GC'd (if the commit doc has the same hash, the guard is preserved).
func (s *Service) DeletePending(ctx context.Context, repoID domain.ContentHash, sessionID string) error {
	if err := domain.ValidateContentHash(repoID); err != nil {
		return err
	}
	old := s.pendingTargetOf(ctx, repoID, sessionID)
	if err := s.meta.DeletePending(ctx, repoID, sessionID); err != nil {
		return err
	}
	s.gcHookLeaf(ctx, repoID, old, "")
	return nil
}

// CompareAndDeletePending resolves only the capture the caller observed.
// Ref-reachable history is never deleted: gcHookLeaf can remove only an
// unreferenced hook leaf replaced by the commit. A concurrent newer pointer
// returns false and remains untouched.
func (s *Service) CompareAndDeletePending(ctx context.Context, repoID domain.ContentHash, sessionID string, expected domain.ContentHash) (bool, error) {
	if err := domain.ValidateContentHash(repoID); err != nil {
		return false, err
	}
	if err := domain.ValidateContentHash(expected); err != nil {
		return false, err
	}
	if sessionID == "" || len(sessionID) > 128 {
		return false, fmt.Errorf("%w: session_id is empty or too long", domain.ErrValidation)
	}
	result, err := s.meta.CompareAndDeletePending(ctx, repoID, sessionID, expected)
	if err != nil {
		return false, err
	}
	if result == domain.PendingDeleteDeleted {
		s.gcHookLeaf(ctx, repoID, expected, "")
	}
	return result.Resolved(), nil
}

// reconcileSharedPendingPointers is a best-effort mutable-metadata cleanup
// after branch/join ref movement. A pending target is resolved only when a
// branch, server-managed session, or immutable branch-lifecycle root reaches
// it through natural or graft parents. Compare-and-delete protects a newer
// capture that arrives during the reachability walk.
func (s *Service) reconcileSharedPendingPointers(ctx context.Context, repoID domain.ContentHash) {
	pendings, err := s.meta.ListPendings(ctx, repoID)
	if err != nil || len(pendings) == 0 {
		return
	}
	refs, err := s.meta.ListRefs(ctx, repoID)
	if err != nil {
		return
	}
	roots := make([]domain.ContentHash, 0, len(refs))
	for _, ref := range refs {
		if sharedTimelineRef(ref) {
			roots = append(roots, ref.Target)
		}
	}
	if len(roots) == 0 {
		return
	}
	snaps, err := s.meta.ListSnapshots(ctx, repoID, "")
	if err != nil {
		return
	}
	parentsOf := make(map[domain.ContentHash][]domain.ContentHash, len(snaps))
	for _, snap := range snaps {
		parentsOf[snap.ID] = snap.ReachabilityParents()
	}
	shared := make(map[domain.ContentHash]bool, len(snaps))
	stack := append([]domain.ContentHash(nil), roots...)
	for len(stack) > 0 {
		id := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if id == "" || shared[id] {
			continue
		}
		shared[id] = true
		stack = append(stack, parentsOf[id]...)
	}
	for _, p := range pendings {
		if !shared[p.Target] {
			continue
		}
		// The target is already ref-reachable, so no hook-leaf GC is needed (or
		// allowed): immutable history remains and only the mutable pointer leaves.
		_, _ = s.meta.CompareAndDeletePending(ctx, repoID, p.SessionID, p.Target)
	}
}

func sharedTimelineRef(ref domain.Ref) bool {
	if ref.Target == "" {
		return false
	}
	if ref.Kind == domain.RefBranch || ref.Kind == domain.RefSession {
		return true
	}
	_, lifecycle, err := domain.ParseBranchLifecycleRef(ref)
	return err == nil && lifecycle
}

// pendingTargetOf returns the current pending target of the session (empty string if none).
func (s *Service) pendingTargetOf(ctx context.Context, repoID domain.ContentHash, sessionID string) domain.ContentHash {
	pendings, err := s.meta.ListPendings(ctx, repoID)
	if err != nil {
		return ""
	}
	for _, p := range pendings {
		if p.SessionID == sessionID {
			return p.Target
		}
	}
	return ""
}

// gcHookLeaf removes the hook capture leaf snapshot (+doc) replaced by sliding or release.
// Only removes if all guards pass: hook prefix message · does not point to any ref · not the target of another pending
// · different from the new object. Hook leaves are always leaves, so they cannot be targets in the commit history.
func (s *Service) gcHookLeaf(ctx context.Context, repoID domain.ContentHash, old, current domain.ContentHash) {
	if old == "" || old == current {
		return
	}
	snap, err := s.meta.GetSnapshot(ctx, repoID, old)
	if err != nil || !strings.HasPrefix(snap.Message, domain.HookMessagePrefix) {
		return
	}
	refs, err := s.meta.ListRefs(ctx, repoID)
	if err != nil {
		return
	}
	// Reachability guard: preserves if reachable from any ref (direct target or ancestor). Previously, only direct targets (r.Target==old) were considered,
	// leading to dangling parent hooks in the lineage when ancestors were deleted (violating invariant R). Safely preserves on calculation failure.
	// Hook capture hot path (in PutPending/DeletePending) — instead of querying store nodes individually (AncestorsClosure, one SQL per ancestor in PG),
	// determines with one ListSnapshots and in-memory walk (like Fsck).
	snaps, lerr := s.meta.ListSnapshots(ctx, repoID, "")
	if lerr != nil {
		return
	}
	parentsOf := make(map[domain.ContentHash][]domain.ContentHash, len(snaps))
	for _, sn := range snaps {
		parentsOf[sn.ID] = sn.ReachabilityParents()
	}
	seen := map[domain.ContentHash]bool{}
	var stack []domain.ContentHash
	for _, r := range refs {
		if r.Target != "" {
			stack = append(stack, r.Target)
		}
	}
	for len(stack) > 0 {
		n := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if n == "" || seen[n] {
			continue
		}
		if n == old {
			return // ref reachable — preserve
		}
		seen[n] = true
		stack = append(stack, parentsOf[n]...)
	}
	pendings, err := s.meta.ListPendings(ctx, repoID)
	if err != nil {
		return
	}
	for _, p := range pendings {
		if p.Target == old {
			return
		}
	}
	if err := s.meta.DeleteSnapshot(ctx, repoID, old); err != nil {
		return
	}
	_ = s.blobs.DeleteDoc(ctx, repoID, snap.DocHash)
}

// PutUnsync updates (user, branch) push wait pointers (shadow sync mirror).
// user/branch are authoritative — overwrites body value. If ref is already target,
// it's effectively synced, so instead of upsert, it resolves (deletes).
func (s *Service) PutUnsync(ctx context.Context, repoID domain.ContentHash, user, branch string, u domain.Unsync) error {
	if err := domain.ValidateContentHash(repoID); err != nil {
		return err
	}
	if user == "" || branch == "" {
		return fmt.Errorf("%w: user/branch required", domain.ErrIntegrity)
	}
	if err := domain.ValidateBranchName(branch); err != nil {
		return err
	}
	if u.Target == "" {
		return fmt.Errorf("%w: unsync.target (snapshot hash) required", domain.ErrIntegrity)
	}
	if err := domain.ValidateContentHash(u.Target); err != nil {
		return err
	}
	// If server ref includes target, local is no longer ahead → resolves.
	// Includes when ref is descendant (push forward) of target — stale instance
	// late-revives unsync, self-healing (review backlog #2 defense-in-depth).
	if ref, err := s.meta.GetRef(ctx, repoID, domain.RefBranch, branch); err == nil {
		if reached, aerr := s.engine.IsAncestor(ctx, repoID, u.Target, ref.Target); aerr == nil && reached {
			return s.meta.DeleteUnsync(ctx, repoID, user, branch)
		}
	}
	u.RepoID = repoID
	u.User = user
	u.Branch = branch
	u.UpdatedAt = time.Now().UTC()
	return s.meta.PutUnsync(ctx, repoID, u)
}

// ListUnsyncs returns the entire push wait pointer set for the repo (for web On Hold display).
func (s *Service) ListUnsyncs(ctx context.Context, repoID domain.ContentHash) ([]domain.Unsync, error) {
	if err := domain.ValidateContentHash(repoID); err != nil {
		return nil, err
	}
	return s.meta.ListUnsyncs(ctx, repoID)
}

// DeleteUnsync resolves a push wait pointer (git push/manual cleanup — idempotent).
func (s *Service) DeleteUnsync(ctx context.Context, repoID domain.ContentHash, user, branch string) error {
	if err := domain.ValidateContentHash(repoID); err != nil {
		return err
	}
	if err := domain.ValidateBranchName(branch); err != nil {
		return err
	}
	return s.meta.DeleteUnsync(ctx, repoID, user, branch)
}

const secretsKDFIterations = 600_000

type secretsEnvelope struct {
	Version       int    `json:"version"`
	KDF           string `json:"kdf"`
	Iterations    int    `json:"iterations"`
	SaltB64       string `json:"salt_b64"`
	Cipher        string `json:"cipher"`
	NonceB64      string `json:"nonce_b64"`
	CiphertextB64 string `json:"ciphertext_b64"`
	Fingerprint   string `json:"fingerprint,omitempty"`
}

func validateSecretsEnvelope(raw []byte) error {
	var env secretsEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("%w: invalid secrets envelope JSON", domain.ErrIntegrity)
	}
	if env.Version != 1 || env.KDF != "PBKDF2-SHA256" || env.Cipher != "AES-256-GCM" || env.Iterations != secretsKDFIterations {
		return fmt.Errorf("%w: unsupported secrets envelope parameters", domain.ErrIntegrity)
	}
	salt, err := base64.StdEncoding.DecodeString(env.SaltB64)
	if err != nil || len(salt) != 16 {
		return fmt.Errorf("%w: secrets envelope salt must be 16 bytes", domain.ErrIntegrity)
	}
	nonce, err := base64.StdEncoding.DecodeString(env.NonceB64)
	if err != nil || len(nonce) != 12 {
		return fmt.Errorf("%w: secrets envelope nonce must be 12 bytes", domain.ErrIntegrity)
	}
	ciphertext, err := base64.StdEncoding.DecodeString(env.CiphertextB64)
	if err != nil || len(ciphertext) < 16 {
		return fmt.Errorf("%w: invalid AES-GCM ciphertext", domain.ErrIntegrity)
	}
	if env.Fingerprint != "" {
		if len(env.Fingerprint) != 12 {
			return fmt.Errorf("%w: invalid secrets fingerprint", domain.ErrIntegrity)
		}
		for _, r := range env.Fingerprint {
			if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
				return fmt.Errorf("%w: invalid secrets fingerprint", domain.ErrIntegrity)
			}
		}
	}
	return nil
}

// PutSecrets stores a secret ciphertext envelope. Server never decrypts (E2E),
// but client validates format to prevent team members from forcing abnormal KDF cost or AES-GCM parameters.
func (s *Service) PutSecrets(ctx context.Context, repoID domain.ContentHash, raw []byte) error {
	if err := domain.ValidateContentHash(repoID); err != nil {
		return err
	}
	if len(raw) > 256<<10 {
		return fmt.Errorf("%w: envelope exceeds 256KB", domain.ErrIntegrity)
	}
	if err := validateSecretsEnvelope(raw); err != nil {
		return err
	}
	if err := s.meta.PutSecretsEnvelope(ctx, repoID, raw); err != nil {
		return err
	}
	s.notifySecretsChanged(ctx, repoID) // best-effort notification — no impact on save success
	return nil
}

// GetSecrets returns secret ciphertext envelopes (ciphertext as-is — decryption is on the client).
func (s *Service) GetSecrets(ctx context.Context, repoID domain.ContentHash) ([]byte, error) {
	if err := domain.ValidateContentHash(repoID); err != nil {
		return nil, err
	}
	return s.meta.GetSecretsEnvelope(ctx, repoID)
}

// PutSettingsObject stores a commit attachment settings object (path/size validation, content-addressed idempotency).
func (s *Service) PutSettingsObject(ctx context.Context, repoID domain.ContentHash, hash domain.ContentHash, bundle domain.SettingsBundle) error {
	if err := validateHashes(repoID, hash); err != nil {
		return err
	}
	if err := domain.ValidateSettingsBundle(bundle.Kind, hash, bundle); err != nil {
		return err
	}
	return s.meta.PutSettingsObject(ctx, repoID, hash, bundle)
}

// GetSettingsObjectByHash retrieves a commit attachment settings object by hash.
func (s *Service) GetSettingsObjectByHash(ctx context.Context, repoID domain.ContentHash, hash domain.ContentHash) (domain.SettingsBundle, error) {
	if err := validateHashes(repoID, hash); err != nil {
		return domain.SettingsBundle{}, err
	}
	return s.meta.GetSettingsObject(ctx, repoID, hash)
}

// GetSettings retrieves setting bundles.
func (s *Service) GetSettings(ctx context.Context, repoID domain.ContentHash, kind string) (domain.SettingsBundle, error) {
	if err := domain.ValidateContentHash(repoID); err != nil {
		return domain.SettingsBundle{}, err
	}
	if !settingsKindOK(kind) {
		return domain.SettingsBundle{}, domain.ErrNotFound
	}
	return s.meta.GetSettingsBundle(ctx, repoID, kind)
}

// pathSegs splits a URL path into a slice of non-empty segment strings.
func pathSegs(p string) []string {
	p = strings.Trim(p, "/")
	if p == "" {
		return nil
	}
	return strings.Split(p, "/")
}

// workspaceForURL extracts the first two path segments of a remote URL as (owner_username, slug) and finds the workspace. The server does not create unowned repos.
func (s *Service) workspaceForURL(ctx context.Context, remoteURL string) (string, error) {
	if s.ws == nil {
		return "", fmt.Errorf("%w: workspace binding store unavailable", domain.ErrForbidden)
	}
	remoteURL = strings.TrimSpace(remoteURL)
	if remoteURL == "" {
		return "", fmt.Errorf("%w: repo remote_url is required", domain.ErrValidation)
	}
	u, err := url.Parse(remoteURL)
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("%w: repo remote_url must be an absolute URL: %q", domain.ErrValidation, remoteURL)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("%w: repo remote_url scheme must be http or https: %q", domain.ErrValidation, remoteURL)
	}
	seg := pathSegs(u.Path)
	if len(seg) != 2 {
		return "", fmt.Errorf("%w: repo URL must be <host>/<username>/<workspace> (2-segment): %q", domain.ErrValidation, remoteURL)
	}
	wsp, err := s.ws.GetWorkspaceByPath(ctx, seg[0], seg[1])
	if err != nil {
		return "", fmt.Errorf("%w: repo remote_url does not match an existing workspace: %q", domain.ErrForbidden, remoteURL)
	}
	return wsp.ID, nil
}

// normalizeAboutWebsite ensures that the About website href is stored as an XSS-safe absolute URL.
func normalizeAboutWebsite(raw string) (string, error) {
	v := strings.TrimSpace(raw)
	if v == "" {
		return "", nil
	}
	if len(v) > 2048 {
		return "", fmt.Errorf("%w: website URL is too long", domain.ErrValidation)
	}
	for _, r := range v {
		if r <= 0x1f || r == 0x7f || unicode.IsSpace(r) || r == '\\' {
			return "", fmt.Errorf("%w: website must be a single-line URL", domain.ErrValidation)
		}
	}
	if !strings.Contains(v, "://") {
		v = "https://" + v
	}
	u, err := url.Parse(v)
	if err != nil || u.Host == "" || u.Hostname() == "" || u.Opaque != "" {
		return "", fmt.Errorf("%w: website must be a valid URL", domain.ErrValidation)
	}
	u.Scheme = strings.ToLower(u.Scheme)
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("%w: website scheme must be http or https", domain.ErrValidation)
	}
	if u.User != nil {
		return "", fmt.Errorf("%w: website URL must not contain credentials", domain.ErrValidation)
	}
	u.Host = strings.ToLower(u.Host)
	if u.Path == "/" && u.RawQuery == "" && u.Fragment == "" {
		u.Path = ""
	}
	normalized := u.String()
	if len(normalized) > 2048 {
		return "", fmt.Errorf("%w: website URL is too long", domain.ErrValidation)
	}
	return normalized, nil
}
func (s *Service) ListRepos(ctx context.Context, team string) ([]domain.Repo, error) {
	return s.meta.ListRepos(ctx, team)
}

// Activity groups the "Contribution activity" feed by month (latest month first):
// Monthly context commit bundles (workspace counts) + workspaces created that month.
// Workspaces are already filtered by visibility in the caller (PublicUser).
func (s *Service) Activity(ctx context.Context, workspaces []domain.Workspace) ([]domain.ActivityMonth, error) {
	wsByID := make(map[string]domain.Workspace, len(workspaces))
	for _, w := range workspaces {
		wsByID[w.ID] = w
	}
	repos, err := s.meta.ListRepos(ctx, "default")
	if err != nil {
		return nil, err
	}
	commits := map[string]map[string]int{} // month -> wsID -> count
	for _, r := range repos {
		if _, ok := wsByID[r.WorkspaceID]; !ok {
			continue
		}
		snaps, err := s.meta.ListSnapshots(ctx, r.ID, "")
		if err != nil {
			return nil, err
		}
		for _, sn := range snaps {
			if sn.CreatedAt.IsZero() || strings.HasPrefix(sn.Message, domain.HookMessagePrefix) || sn.Branch == domain.StashBranchLabel {
				continue
			}
			m := sn.CreatedAt.UTC().Format("2006-01")
			if commits[m] == nil {
				commits[m] = map[string]int{}
			}
			commits[m][r.WorkspaceID]++
		}
	}
	created := map[string][]domain.Workspace{} // month -> workspaces created
	for _, w := range workspaces {
		if !w.CreatedAt.IsZero() {
			created[w.CreatedAt.UTC().Format("2006-01")] = append(created[w.CreatedAt.UTC().Format("2006-01")], w)
		}
	}
	monthSet := map[string]bool{}
	for m := range commits {
		monthSet[m] = true
	}
	for m := range created {
		monthSet[m] = true
	}
	months := make([]string, 0, len(monthSet))
	for m := range monthSet {
		months = append(months, m)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(months))) // latest month first

	path := func(w domain.Workspace) string { return w.OwnerUsername + "/" + w.Slug }
	out := make([]domain.ActivityMonth, 0, len(months))
	for _, m := range months {
		am := domain.ActivityMonth{Month: m}
		for wsID, c := range commits[m] {
			w := wsByID[wsID]
			am.CommitTotal += c
			am.CommitRepos = append(am.CommitRepos, domain.ActivityRepo{Name: w.Name, Path: path(w), Count: c})
		}
		sort.Slice(am.CommitRepos, func(i, j int) bool { return am.CommitRepos[i].Count > am.CommitRepos[j].Count })
		cws := created[m]
		sort.Slice(cws, func(i, j int) bool { return cws[i].CreatedAt.After(cws[j].CreatedAt) })
		for _, w := range cws {
			vis := string(w.Visibility)
			if vis == "" {
				vis = "private"
			}
			am.Created = append(am.Created, domain.ActivityCreated{
				Name: w.Name, Path: path(w), Visibility: vis, Date: w.CreatedAt.UTC().Format("2006-01-02"),
			})
		}
		out = append(out, am)
	}
	return out, nil
}

// Contributions aggregates repo commits by date (YYYY-MM-DD, UTC) for the given workspaces
// (for contribution heatmap). Hooks and stash are excluded — only actual context commits are counted.
// Note: repo/snapshot full traversal, inefficient for large-scale — assumes small self-hosted (future DB aggregation optimization possible).
func (s *Service) Contributions(ctx context.Context, workspaceIDs []string) (map[string]int, error) {
	out := map[string]int{}
	if len(workspaceIDs) == 0 {
		return out, nil
	}
	wanted := make(map[string]bool, len(workspaceIDs))
	for _, id := range workspaceIDs {
		wanted[id] = true
	}
	repos, err := s.meta.ListRepos(ctx, "default")
	if err != nil {
		return nil, err
	}
	for _, r := range repos {
		if !wanted[r.WorkspaceID] {
			continue
		}
		snaps, err := s.meta.ListSnapshots(ctx, r.ID, "")
		if err != nil {
			return nil, err
		}
		for _, sn := range snaps {
			if sn.CreatedAt.IsZero() || strings.HasPrefix(sn.Message, domain.HookMessagePrefix) || sn.Branch == domain.StashBranchLabel {
				continue
			}
			out[sn.CreatedAt.UTC().Format("2006-01-02")]++
		}
	}
	return out, nil
}
func (s *Service) GetRepo(ctx context.Context, id domain.ContentHash) (domain.Repo, error) {
	if err := domain.ValidateContentHash(id); err != nil {
		return domain.Repo{}, err
	}
	return s.meta.GetRepo(ctx, id)
}
func (s *Service) GetSnapshot(ctx context.Context, repoID, id domain.ContentHash) (domain.Snapshot, error) {
	if err := validateHashes(repoID, id); err != nil {
		return domain.Snapshot{}, err
	}
	return s.meta.GetSnapshot(ctx, repoID, id)
}
func (s *Service) GetDoc(ctx context.Context, repoID, hash domain.ContentHash) (domain.SessionDoc, error) {
	if err := validateHashes(repoID, hash); err != nil {
		return domain.SessionDoc{}, err
	}
	return s.blobs.GetDoc(ctx, repoID, hash)
}
func (s *Service) ListRefs(ctx context.Context, repoID domain.ContentHash) ([]domain.Ref, error) {
	if err := domain.ValidateContentHash(repoID); err != nil {
		return nil, err
	}
	return s.meta.ListRefs(ctx, repoID)
}

// Fork creates a new branch ref with tip FromSnapshot (O(1), original unchanged F1).
func (s *Service) Fork(ctx context.Context, in inbound.ForkInput) (inbound.ForkOutput, error) {
	if err := validateHashes(in.RepoID, in.FromSnapshot); err != nil {
		return inbound.ForkOutput{}, err
	}
	if err := domain.ValidateBranchName(in.NewBranch); err != nil {
		return inbound.ForkOutput{}, err
	}
	if _, err := s.meta.GetSnapshot(ctx, in.RepoID, in.FromSnapshot); err != nil {
		return inbound.ForkOutput{}, err // parent snapshot pre-exists (REF1)
	}
	ref := domain.Ref{Kind: domain.RefBranch, Name: in.NewBranch, RepoID: in.RepoID, Target: in.FromSnapshot}
	if err := s.meta.CompareAndSwapRef(ctx, in.RepoID, ref, ""); err != nil {
		return inbound.ForkOutput{}, err // already exists, ErrRefConflict (F2)
	}
	return inbound.ForkOutput{Branch: in.NewBranch, Head: in.FromSnapshot}, nil
}

// Join moves commit X of the same git branch session fork (sibling branch) to behind the branch head (graph drag&drop). The context session is tied to its git branch — no merge into other branches (enforced by projected branch membership).
//
// Full graft register + ref move — parent original unchanged, history rewritten/lost none:
//  1. X with natural parent only in H history is rejected (true history — reordering meaningless·circular source)
//  2. X follows unique first-parent child to tip, server calculates tip (SessionID only marks boundary)
//  3. Existing graft in-flow edge superseded, X grafts to H, optional session ref, branch ref CAS to one ApplyJoin change set
//
// PostgreSQL uses transaction+row lock, FS uses durable intent journal+replay for mid-failure recovery. Supersede recorded before new edge in FS prevents circularity even in mid-state.
func (s *Service) Join(ctx context.Context, in inbound.JoinInput) (inbound.JoinOutput, error) {
	if err := validateHashes(in.RepoID, in.Snapshot); err != nil {
		return inbound.JoinOutput{}, err
	}
	if err := domain.ValidateBranchName(in.TargetBranch); err != nil {
		return inbound.JoinOutput{}, err
	}
	if in.TargetBranch == "HEAD" {
		return inbound.JoinOutput{}, fmt.Errorf("%w: HEAD is not a joinable branch", domain.ErrValidation)
	}
	snaps, err := s.meta.ListSnapshots(ctx, in.RepoID, "")
	if err != nil {
		return inbound.JoinOutput{}, err
	}
	byID := make(map[domain.ContentHash]domain.Snapshot, len(snaps))
	for _, sn := range snaps {
		byID[sn.ID] = sn
	}
	refs, err := s.meta.ListRefs(ctx, in.RepoID)
	if err != nil {
		return inbound.JoinOutput{}, err
	}
	// Writes the same commit set as the UI. Active pending target is never a join target, and hook leafs not reachable from ref are excluded from descendant/tip calculation.
	// Conversely, past deduplication leaves hook labels, but if reachable from a branch/session/lifecycle root, it's already a shared commit and included.
	shared := map[domain.ContentHash]bool{}
	targetSessionShared := map[domain.ContentHash]bool{}
	for _, ref := range refs {
		if sharedTimelineRef(ref) {
			for id := range snapshotReachableSet(byID, ref.Target) {
				shared[id] = true
				if ref.Kind == domain.RefSession && strings.HasPrefix(ref.Name, domain.SessionRefPrefix(in.TargetBranch)) {
					targetSessionShared[id] = true
				}
			}
		}
	}
	pendingTargets := map[domain.ContentHash]bool{}
	pendings, err := s.meta.ListPendings(ctx, in.RepoID)
	if err != nil {
		return inbound.JoinOutput{}, err
	}
	for _, pending := range pendings {
		// Applies rules like web's uncommitted determination. Targets already reachable from branch/session ref are stale pending and dismissed is user's pointer in progress list. Both must not block normal commit join.
		if pending.Dismissed || shared[pending.Target] {
			continue
		}
		pendingTargets[pending.Target] = true
	}
	isJoinCommit := func(id domain.ContentHash) bool {
		snap, ok := byID[id]
		return ok && !pendingTargets[id] && (!strings.HasPrefix(snap.Message, domain.HookMessagePrefix) || shared[id])
	}
	firstChildren := map[domain.ContentHash][]domain.ContentHash{}
	for _, sn := range snaps {
		if isJoinCommit(sn.ID) && len(sn.Parents) > 0 {
			firstChildren[sn.Parents[0]] = append(firstChildren[sn.Parents[0]], sn.ID)
		}
	}
	members, err := s.snapshotBranchMemberships(ctx, in.RepoID, snaps)
	if err != nil {
		return inbound.JoinOutput{}, err
	}
	snapX, ok := byID[in.Snapshot]
	if !ok {
		return inbound.JoinOutput{}, fmt.Errorf("%w: snapshot %s", domain.ErrNotFound, in.Snapshot)
	}
	if !isJoinCommit(in.Snapshot) {
		return inbound.JoinOutput{}, fmt.Errorf("%w: pending hook capture cannot be joined", domain.ErrValidation)
	}
	if !members[in.Snapshot][in.TargetBranch] {
		return inbound.JoinOutput{}, fmt.Errorf("%w: snapshot does not belong to git branch %q — cross-branch join is not allowed", domain.ErrConflict, in.TargetBranch)
	}
	head, err := s.meta.GetRef(ctx, in.RepoID, domain.RefBranch, in.TargetBranch)
	if err != nil || head.Target == "" {
		return inbound.JoinOutput{}, fmt.Errorf("%w: target branch %q has no head", domain.ErrNotFound, in.TargetBranch)
	}
	if head.Target == in.Snapshot {
		return inbound.JoinOutput{}, fmt.Errorf("%w: snapshot is already the branch head", domain.ErrConflict)
	}
	headReach := snapshotReachableSet(byID, head.Target)
	targetShared := make(map[domain.ContentHash]bool, len(headReach)+len(targetSessionShared))
	for id := range headReach {
		targetShared[id] = true
	}
	for id := range targetSessionShared {
		targetShared[id] = true
	}
	// Join is an operation to reorder a shared session branch. It's not a bypass path for objects-only shadow push, which could elevate unpushed objects or past dangling snapshots to branch ref on web. Only allow in target branch's graft reach set or partial join session ref.
	if !targetShared[in.Snapshot] {
		return inbound.JoinOutput{}, fmt.Errorf("%w: snapshot is not an attached session branch; push it first", domain.ErrConflict)
	}
	// 1) Natural history determination: parents-only walk excluding grafts — branches connected only by grafts are reordering targets, natural history rejects (head retreat/circular source).
	if naturalReachable(byID, head.Target, in.Snapshot) {
		return inbound.JoinOutput{}, fmt.Errorf("%w: snapshot already in branch %q natural history", domain.ErrConflict, in.TargetBranch)
	}
	// 2) Segments are not SessionID but natural first-parent path. Server extends from X to target git branch's unique child leaf. Multiple children mean unknown lane, so safely reject. Client doesn't specify move range/tip.
	segment := map[domain.ContentHash]bool{}
	branchChildren := func(id domain.ContentHash) []domain.ContentHash {
		var out []domain.ContentHash
		for _, kid := range firstChildren[id] {
			if members[kid][in.TargetBranch] {
				out = append(out, kid)
			}
		}
		return out
	}
	tip := in.Snapshot
	segment[tip] = true
	segmentIDs := []domain.ContentHash{tip}
	seen := map[domain.ContentHash]bool{tip: true}
	for {
		kids := branchChildren(tip)
		if len(kids) == 0 {
			break
		}
		if len(kids) > 1 {
			return inbound.JoinOutput{}, fmt.Errorf("%w: chain above snapshot forks — join a leaf after resolving the fork", domain.ErrConflict)
		}
		tip = kids[0]
		// Objects-only shadow push existing natural descendants elevate to "full join" branch/session ref, bypassing cxt push public boundary. Opposite full join must be reachable from current branch or partial join session ref, i.e., pushed commits.
		if !targetShared[tip] {
			return inbound.JoinOutput{}, fmt.Errorf("%w: chain above snapshot contains an unpushed commit; push it first", domain.ErrConflict)
		}
		if seen[tip] {
			return inbound.JoinOutput{}, fmt.Errorf("%w: cycle in natural lineage", domain.ErrIntegrity)
		}
		seen[tip] = true
		segment[tip] = true
		segmentIDs = append(segmentIDs, tip)
	}
	newHead := in.Snapshot
	if in.IncludeDescendants {
		newHead = tip
	}
	// 3) Supersede plan: Remove graft in-flow edge from outside to inside segment (auto-graft residue) to prevent reordering loop. Segment reachability continues with new head/session ref, so total reach set doesn't shrink.
	type patch struct {
		id   domain.ContentHash
		next []domain.ContentHash
	}
	var supersedes []patch
	// graft register is snapshot global meta, if another git branch ref reaches the same source, edge removal changes the branch graph. Shared sources cannot be safely superseded without branch-scoped placement, so it is rejected.
	otherBranchReach := map[domain.ContentHash]bool{}
	targetSessionPrefix := domain.SessionRefPrefix(in.TargetBranch)
	for _, ref := range refs {
		otherScope := (ref.Kind == domain.RefBranch && ref.Name != in.TargetBranch) ||
			(ref.Kind == domain.RefSession && !strings.HasPrefix(ref.Name, targetSessionPrefix))
		if !otherScope || ref.Target == "" {
			continue
		}
		for id := range snapshotReachableSet(byID, ref.Target) {
			otherBranchReach[id] = true
		}
	}
	for _, id := range segmentIDs {
		if otherBranchReach[id] {
			return inbound.JoinOutput{}, fmt.Errorf("%w: snapshot %s is currently reachable from another git branch", domain.ErrConflict, id)
		}
	}
	for _, sn := range snaps {
		if segment[sn.ID] || len(sn.GraftParents) == 0 || !headReach[sn.ID] {
			continue
		}
		var next []domain.ContentHash
		hit := false
		for _, g := range sn.GraftParents {
			if segment[g] {
				hit = true
				continue
			}
			next = append(next, g)
		}
		if hit {
			if !members[sn.ID][in.TargetBranch] {
				return inbound.JoinOutput{}, fmt.Errorf("%w: a graft from another git branch blocks this join", domain.ErrConflict)
			}
			if otherBranchReach[sn.ID] {
				return inbound.JoinOutput{}, fmt.Errorf("%w: graft source %s is shared by another git branch", domain.ErrConflict, sn.ID)
			}
			supersedes = append(supersedes, patch{id: sn.ID, next: next})
		}
	}
	out := inbound.JoinOutput{Branch: in.TargetBranch}
	// 4) Repository atomic change set composition. supersede first, then X's new edge last. FS implementation also avoids creating loops during mid-crash, and PostgreSQL is a single transaction.
	var forkName string
	if !in.IncludeDescendants && tip != in.Snapshot {
		var ferr error
		forkName, ferr = s.joinForkRefName(ctx, in.RepoID, in.TargetBranch, tip)
		if ferr != nil {
			return inbound.JoinOutput{}, ferr
		}
		out.ForkBranch = forkName
	}
	patches := make([]outbound.GraftPatch, 0, len(supersedes)+1)
	for _, p := range supersedes {
		patches = append(patches, outbound.GraftPatch{SnapshotID: p.id, ExpectedSeq: byID[p.id].GraftSeq, Parents: p.next})
	}
	xNext := append([]domain.ContentHash{}, snapX.GraftParents...)
	foundHead := false
	for _, parent := range xNext {
		foundHead = foundHead || parent == head.Target
	}
	if !foundHead {
		xNext = append(xNext, head.Target)
	}
	patches = append(patches, outbound.GraftPatch{SnapshotID: in.Snapshot, ExpectedSeq: snapX.GraftSeq, Parents: xNext})
	forkTip := domain.ContentHash("")
	if forkName != "" {
		forkTip = tip
	}
	if err := s.meta.ApplyJoin(ctx, outbound.JoinMutation{
		RepoID: in.RepoID, Branch: in.TargetBranch, Source: in.Snapshot, Segment: segmentIDs,
		ExpectedHead: head.Target, NewHead: newHead,
		ForkName: forkName, ForkTip: forkTip, Grafts: patches,
	}); err != nil {
		return inbound.JoinOutput{}, err
	}
	out.Head = newHead
	s.reconcileSharedPendingPointers(ctx, in.RepoID)
	return out, nil
}

func snapshotReachableSet(byID map[domain.ContentHash]domain.Snapshot, head domain.ContentHash) map[domain.ContentHash]bool {
	out := map[domain.ContentHash]bool{}
	stack := []domain.ContentHash{head}
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if cur == "" || out[cur] {
			continue
		}
		out[cur] = true
		if snap, ok := byID[cur]; ok {
			stack = append(stack, snap.ReachabilityParents()...)
		}
	}
	return out
}

// naturalReachable determines if from follows natural parents to anc (excluding grafts).
func naturalReachable(byID map[domain.ContentHash]domain.Snapshot, from, anc domain.ContentHash) bool {
	if from == anc {
		return true
	}
	seen := map[domain.ContentHash]bool{}
	stack := []domain.ContentHash{from}
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if cur == anc {
			return true
		}
		if seen[cur] {
			continue
		}
		seen[cur] = true
		stack = append(stack, byID[cur].Parents...)
	}
	return false
}

// joinForkRefName selects a session ref name to preserve natural descendants after partial join. The session ref itself is not a real git branch membership, but the branch scope in the name participates in join ownership determination. Actual creation and conflict final determination is performed in the ApplyJoin atomic boundary.
func (s *Service) joinForkRefName(ctx context.Context, repoID domain.ContentHash, branch string, tip domain.ContentHash) (string, error) {
	short := strings.TrimPrefix(string(tip), "sha256:")
	if len(short) > 10 {
		short = short[:10]
	}
	base := domain.SessionRefPrefix(branch) + short
	for i := 0; i < 10; i++ {
		name := base
		if i > 0 {
			name = fmt.Sprintf("%s-%d", base, i+1)
		}
		if existing, err := s.meta.GetRef(ctx, repoID, domain.RefSession, name); err == nil {
			if existing.Target == tip {
				return name, nil
			}
			continue
		} else if !errors.Is(err, domain.ErrNotFound) {
			return "", fmt.Errorf("lookup join session ref %q: %w", name, err)
		}
		return name, nil
	}
	return "", fmt.Errorf("%w: fork branch name exhausted for %s", domain.ErrConflict, tip)
}

// PromoteMergedPR converts GitHub PR merge webhook to context promotion:
// Find repos whose Git origin (GitRemoteURL) matches, then append the head branch's cxt tip to the base branch using a lossless graft, the same operation as the post-merge hook. The server covers squash and rebase-merge flows where the local [git sha] mapping disappears (audit finding #14). This is a no-op when the head branch has no context. It returns the number of promoted repos.
func (s *Service) PromoteMergedPR(ctx context.Context, gitURL, baseBranch, headBranch string) (int, error) {
	if gitURL == "" || baseBranch == "" || headBranch == "" || baseBranch == headBranch {
		return 0, nil
	}
	if err := domain.ValidateBranchName(baseBranch); err != nil {
		return 0, err
	}
	if err := domain.ValidateBranchName(headBranch); err != nil {
		return 0, err
	}
	want := normalizeGitURL(gitURL)
	repos, err := s.meta.ListRepos(ctx, "default")
	if err != nil {
		return 0, err
	}
	promoted := 0
	for _, r := range repos {
		if r.GitRemoteURL == "" || normalizeGitURL(r.GitRemoteURL) != want {
			continue
		}
		headRef, gerr := s.meta.GetRef(ctx, r.ID, domain.RefBranch, headBranch)
		if gerr != nil || headRef.Target == "" {
			continue // no head branch context in this repo
		}
		ref := domain.Ref{Kind: domain.RefBranch, Name: baseBranch, RepoID: r.ID, Target: headRef.Target}
		out, uerr := s.UpdateRef(ctx, inbound.UpdateRefInput{RepoID: r.ID, Ref: ref, Append: true})
		if uerr != nil {
			// behind (already reflected)/local hook precedence is idempotent no-op — errors only for others.
			if errors.Is(uerr, domain.ErrNonFastForward) {
				continue
			}
			return promoted, uerr
		}
		if out.Result != inbound.RefUpToDate {
			promoted++
		}
	}
	return promoted, nil
}

// Diff calculates the CIR event delta (add/remove) between two snapshots based on content keys.
func (s *Service) Diff(ctx context.Context, in inbound.DiffInput) (inbound.DiffOutput, error) {
	if err := validateHashes(in.RepoID, in.Left, in.Right); err != nil {
		return inbound.DiffOutput{}, err
	}
	left, err := s.blobs.GetDoc(ctx, in.RepoID, in.Left)
	if err != nil {
		return inbound.DiffOutput{}, err
	}
	right, err := s.blobs.GetDoc(ctx, in.RepoID, in.Right)
	if err != nil {
		return inbound.DiffOutput{}, err
	}
	return inbound.DiffOutput{Changes: diffEvents(left.CIR.Events, right.CIR.Events)}, nil
}

// eventKey is the content equality key for events (seq agnostic, content-based).
func eventKey(e domain.CIREvent) string {
	switch e.Kind {
	case domain.EventMessage, domain.EventTurn:
		t := ""
		if len(e.Blocks) > 0 {
			t = e.Blocks[0].Text
		}
		return string(e.Kind) + "|" + string(e.Role) + "|" + e.AgentAuthor + "|" + e.AgentRecipient + "|" + t
	case domain.EventToolCall:
		return string(e.Kind) + "|" + e.ToolName + "|" + e.CallID
	case domain.EventToolResult:
		return string(e.Kind) + "|" + e.CallID
	case domain.EventReasoning:
		return string(e.Kind) + "|" + e.RedactedSummary
	case domain.EventCompaction:
		// Compaction boundaries may have identical opaque payload shapes. Their
		// archival positions are distinct and append-only, so include seq/ts
		// instead of collapsing every boundary to one diff key.
		return fmt.Sprintf("%s|%d|%s", e.Kind, e.Seq, e.TS)
	}
	return string(e.Kind)
}

func eventSummary(e domain.CIREvent) string {
	switch e.Kind {
	case domain.EventMessage, domain.EventTurn:
		t := ""
		if len(e.Blocks) > 0 {
			t = e.Blocks[0].Text
		}
		if e.AgentMessage && e.AgentAuthor != "" {
			return "agent " + e.AgentAuthor + ": " + truncate(t, 60)
		}
		return string(e.Role) + ": " + truncate(t, 60)
	case domain.EventToolCall:
		return "tool " + e.ToolName
	case domain.EventToolResult:
		return "tool result"
	case domain.EventReasoning:
		return "reasoning"
	case domain.EventCompaction:
		return "context compaction"
	}
	return string(e.Kind)
}

func diffEvents(left, right []domain.CIREvent) []inbound.DiffEntry {
	lset := make(map[string]bool, len(left))
	for _, e := range left {
		lset[eventKey(e)] = true
	}
	rset := make(map[string]bool, len(right))
	for _, e := range right {
		rset[eventKey(e)] = true
	}
	var out []inbound.DiffEntry
	for _, e := range right {
		if !lset[eventKey(e)] {
			out = append(out, inbound.DiffEntry{Op: "add", Seq: e.Seq, Summary: eventSummary(e)})
		}
	}
	for _, e := range left {
		if !rset[eventKey(e)] {
			out = append(out, inbound.DiffEntry{Op: "remove", Seq: e.Seq, Summary: eventSummary(e)})
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
