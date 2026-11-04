package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"reflect"
	"sort"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/wnsdy95/cxthub/backend/internal/app/repositoryquery"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

// Service implements all server inbound use-cases and HTTP read-through.
type Service struct {
	pendingProjectionRepos  map[domain.ContentHash]bool
	graphProjectionRepos    map[domain.ContentHash]bool
	graphReadRepos          map[domain.ContentHash]bool
	graphQualificationRepos []domain.ContentHash
	graphQualificationNext  atomic.Uint64
	memoryReadRepos         []domain.ContentHash
	preparedMemoryRepos     map[domain.ContentHash]bool
	preparedMemoryDemands   map[domain.ContentHash]bool
	rootPublication         bool // configured before serving; cannot override release readiness
	branchCache             repositoryquery.BranchCache
	graphCache              repositoryquery.GraphCache
	segmentCache            repositoryquery.SegmentCache
	memoryReadOwner         atomic.Pointer[memoryReadOwner]
	deliveryCache           effectiveMemoryDeliveryCache
	docVerifier             domain.CanonicalDocVerifier
	projectionEvidence      outbound.GraphEvidenceReader
	projectionMemory        outbound.MemoryQueryReader
	docFindFields           outbound.DocFindFieldsStore
	docDisplay              outbound.DocDisplayStore
	docDisplayRead          outbound.DocDisplayReader
	docDisplayMaintenance   outbound.DocDisplayMaintenanceStore
	projectionMemorySource  *domain.RepositoryMemorySourceGeneration
	requireCompleteMemory   bool
	meta                    outbound.MetadataStore
	blobs                   outbound.BlobStore
	auth                    outbound.AuthProvider
	engine                  outbound.GitEngine
	// repositories resolves bindings and current write authority. A missing adapter
	// denies user writes; explicit trusted system jobs may operate without one.
	repositories       outbound.RepositoryStore
	notificationSender outbound.NotificationSender
}

// NewService creates a Service with an outbound port injected.
func NewService(meta outbound.MetadataStore, blobs outbound.BlobStore, auth outbound.AuthProvider, engine outbound.GitEngine, repositories outbound.RepositoryStore) *Service {
	return &Service{meta: meta, blobs: blobs, auth: auth, engine: engine, repositories: repositories}
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
	return domain.ValidateSnapshotHashes(snap)
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

func settingsObjectHash(bundle domain.SettingsBundle) (domain.ContentHash, error) {
	return domain.SettingsObjectHash(bundle)
}

// --- push/pull use-cases ---

// Negotiate: want (server missing parts) = client haves \ server has (sync protocol step A).
func (s *Service) Negotiate(ctx context.Context, in inbound.PushNegotiateInput) (inbound.PushNegotiateOutput, error) {
	return repositoryReadForRepo(ctx, s, in.RepoID, func(ctx context.Context) (inbound.PushNegotiateOutput, error) {
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
		_, asyncDocs := s.blobs.(outbound.DocJobStore)
		_, storedDocVerification := s.blobs.(outbound.StoredDocVerifier)
		return inbound.PushNegotiateOutput{
			DocIdentitiesSupported:          s.DocumentIdentitiesSupported(),
			RootPublicationEnabled:          s.RootPublicationEnabled(),
			PreparedMemoryArchivesSupported: asyncDocs && storedDocVerification,
			AsyncDocsSupported:              asyncDocs,
			SnapshotWants:                   difference(in.SnapshotHaves, haveSnaps),
			DocWants:                        difference(in.DocHaves, haveDocs),
			ChunksSupported:                 true,
			BoundedChunksSupported:          true,
			ChunkFormatsSupported:           []string{domain.ChunkFormatV1, domain.ChunkFormatV2},
			CIRVersionsSupported:            domain.SupportedCIRVersions(),
			ChunkWants:                      chunkWants,
		}, nil
	})
}

// PullChunks returns the request prefix up to the raw upper bound. If the first chunk exceeds the upper bound, it stops with an explicit validation error to preserve the cause instead of the proxy's ambiguous 413.
func (s *Service) PullChunks(ctx context.Context, in inbound.PullChunksInput) (inbound.PullChunksOutput, error) {
	return resourceReadForRepo(ctx, s, in.RepoID, func(ctx context.Context) (inbound.PullChunksOutput, error) {
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
	})
}

// Commit: stores docs in the order of blobs → snapshots (W1). Full integrity verification followed by content-addressed deduplication.
func (s *Service) commit(ctx context.Context, in inbound.CommitInput) (inbound.CommitOutput, error) {
	commands, err := s.documentPublications(DocumentPublicationCompatibility)
	if err != nil {
		return inbound.CommitOutput{}, err
	}
	return commands.commit(ctx, in)
}

// UpdateRef: Update the reference based on the current target and CAS (sync protocol).
//
// Similar to git policy: Branches are allowed to move only in a fast-forward manner (fast-forward), and
// any other move (non-fast-forward/diverged) is rejected with ErrNonFastForward — except for Force.
// Tags are immutable: Moving to another target is rejected without Force.
func (s *Service) updateRefWithPending(ctx context.Context, in inbound.UpdateRefInput) (inbound.UpdateRefOutput, error) {
	return s.repositoryRefs().updateRefWithPending(ctx, in)
}

// UpdateRefs keeps CAS checks and pending reconciliation inside the enclosing
// repository transaction on PostgreSQL. An empty retry is intentional: it repairs a server
// crash that occurred after the last ref write but before metadata cleanup.
func (s *Service) updateRefs(ctx context.Context, in inbound.UpdateRefsInput) (inbound.UpdateRefsOutput, error) {
	return s.repositoryRefs().updateRefs(ctx, in)
}

func (s *Service) updateRef(ctx context.Context, in inbound.UpdateRefInput) (inbound.UpdateRefOutput, error) {
	return s.repositoryRefs().updateRef(ctx, in)
}

// appendDiverged appends a diverged push as an unaltered overlay to the current head.
//
// For incoming chains where the server head is unaware of a segment (segment = ancestry(target) − ancestry(head)), the boundary snapshot's GraftParents are updated to include the current head. All boundary segments, including root parents and shared histories, follow the same rules, and natural parents are never changed.
//
// Safety justification: The segment is disjoint from the head ancestor set and is acyclic. Both existing natural parents and the head are reachable, so reachability is preserved and extended. Replicas with the same ID continue to agree on natural parents.
// Unlike Force, there is no loss, making it non-target of protected branch policies.
// Unlike code, context does not force convergence ("pull is user's responsibility"), so push must always succeed on any divergence — the server DB continues to be updated.
func (s *Service) appendDiverged(ctx context.Context, in inbound.UpdateRefInput, serverTarget domain.ContentHash, out inbound.UpdateRefOutput) (inbound.UpdateRefOutput, error) {
	return s.repositoryRefs().appendDiverged(ctx, in, serverTarget, out)
}

// Send: reply to pull request with missing objects (snapshot meta + doc) (read-only).
func (s *Service) send(ctx context.Context, in inbound.PullSendInput) (inbound.PullSendOutput, error) {
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
		if err := snap.DocumentRef().Validate(); err != nil {
			return inbound.PullSendOutput{}, err
		}
		if err := outbound.CheckDocumentIdentityCompatibility(ctx, snap.DocIdentity); err != nil {
			return inbound.PullSendOutput{}, err
		}
		if snap.DocIdentity == domain.DocumentIdentityRootV1 {
			if err := requireRootDocumentRepository(ctx, s.meta, in.RepoID); err != nil {
				return inbound.PullSendOutput{}, err
			}
		}
		out.Snapshots = append(out.Snapshots, snap)
	}
	for _, h := range in.DocWants {
		if snap, err := s.meta.GetSnapshot(ctx, in.RepoID, h); err == nil {
			if snap.DocIdentity != domain.DocumentIdentityLegacy {
				return inbound.PullSendOutput{}, domain.ErrUnsupportedDocumentIdentity
			}
		} else if !errors.Is(err, domain.ErrNotFound) {
			return inbound.PullSendOutput{}, err
		}
		doc, err := s.blobs.GetDoc(ctx, in.RepoID, h)
		if err != nil {
			return inbound.PullSendOutput{}, err
		}
		if doc.Identity != domain.DocumentIdentityLegacy {
			return inbound.PullSendOutput{}, domain.ErrUnsupportedDocumentIdentity
		}
		if err := requireSupportedCIRVersion(doc.CIR.Envelope.CIRVersion, supportedCIR); err != nil {
			return inbound.PullSendOutput{}, err
		}
		if doc.Identity != domain.DocumentIdentityLegacy {
			return inbound.PullSendOutput{}, domain.ErrUnsupportedDocumentIdentity
		}
		out.Docs = append(out.Docs, doc)
	}
	// Chunk wire (delta download): manifest reply — plan-unavailable doc (ErrNotFound) is a fallback reply (client validates if manifests ∪ docs cover the entire request).
	if err := validateHashes(in.DocManifestWants...); err != nil {
		return inbound.PullSendOutput{}, err
	}
	supportedFormats := requestedChunkFormats(in.ChunkFormatsSupported)
	for _, h := range in.DocManifestWants {
		snap, snapshotErr := s.meta.GetSnapshot(ctx, in.RepoID, h)
		if snapshotErr == nil && snap.DocIdentity != domain.DocumentIdentityLegacy {
			root, err := s.publishedRootRepresentation(ctx, in.RepoID, h, snap, supportedCIR)
			if err != nil {
				return inbound.PullSendOutput{}, err
			}
			out.DocManifests = append(out.DocManifests, root)
			continue
		}
		if snapshotErr != nil {
			if !errors.Is(snapshotErr, domain.ErrNotFound) {
				return inbound.PullSendOutput{}, snapshotErr
			}
			repo, err := s.meta.GetRepo(ctx, in.RepoID)
			if err != nil && !errors.Is(err, domain.ErrNotFound) {
				return inbound.PullSendOutput{}, err
			}
			if repo.RequiredDocIdentity != domain.DocumentIdentityLegacy {
				return inbound.PullSendOutput{}, snapshotErr
			}
		}
		man, merr := s.blobs.GetDocManifest(ctx, in.RepoID, h)
		if merr != nil {
			if errors.Is(merr, domain.ErrNotFound) {
				doc, derr := s.blobs.GetDoc(ctx, in.RepoID, h)
				if derr != nil {
					return inbound.PullSendOutput{}, derr
				}
				if doc.Identity != domain.DocumentIdentityLegacy {
					return inbound.PullSendOutput{}, domain.ErrUnsupportedDocumentIdentity
				}
				if err := requireSupportedCIRVersion(doc.CIR.Envelope.CIRVersion, supportedCIR); err != nil {
					return inbound.PullSendOutput{}, err
				}
				if doc.Identity != domain.DocumentIdentityLegacy {
					return inbound.PullSendOutput{}, domain.ErrUnsupportedDocumentIdentity
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
		if doc.Identity != domain.DocumentIdentityLegacy {
			return inbound.PullSendOutput{}, domain.ErrUnsupportedDocumentIdentity
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
func (s *Service) promoteSnapshotMessageCommand(ctx context.Context, repoID, id domain.ContentHash, message string) error {
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
	return repositoryWriteError(ctx, s, repoID, func(ctx context.Context) error { return s.meta.UpdateSnapshotMessage(ctx, repoID, id, message) })
}

// List: branch-specific snapshot metadata list.
func (s *Service) list(ctx context.Context, in inbound.ListSnapshotsInput) ([]domain.Snapshot, error) {
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
	logs, err := s.meta.ReadReflog(ctx, repoID)
	if err != nil {
		return nil, err
	}
	return domain.ProjectSnapshotBranchMemberships(ctx, snaps, logs)
}

// Fsck: reference reachability audit (read-only, git fsck equivalent). Creates a reachability set by following parents from all refs and pending sessions, then reports unreferenced snapshots, missing parent references (corruption), and parentless roots. Unreferenced snapshots remain stored and are not necessarily corrupt. Does not fix anything — parentless roots are treated as normal roots (not artificially attaching parents).
func (s *Service) fsck(ctx context.Context, repoID domain.ContentHash) (inbound.FsckReport, error) {
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
	archives, err := s.sessionArchiveRecords(ctx, repoID)
	if err != nil {
		return inbound.FsckReport{}, err
	}
	archivedKeys := make(map[domain.ContentHash]bool, len(archives))
	for _, archive := range archives {
		archivedKeys[archive.Key] = true
		stack = append(stack, archive.SnapshotID)
	}
	for _, snapshot := range snaps {
		if archivedKeys[domain.SessionArchiveKey(snapshot)] {
			stack = append(stack, snapshot.ID)
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
	return repositoryReadForRepo(ctx, s, repoID, func(ctx context.Context) ([]domain.RefLogEntry, error) {
		if err := domain.ValidateContentHash(repoID); err != nil {
			return nil, err
		}
		return s.meta.ReadReflog(ctx, repoID)
	})
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

func (s *Service) getManifest(ctx context.Context, repoID domain.ContentHash) (domain.Manifest, error) {
	if err := domain.ValidateContentHash(repoID); err != nil {
		return domain.Manifest{}, err
	}
	return s.meta.GetManifest(ctx, repoID)
}
func (s *Service) ensureRepo(ctx context.Context, actorID string, repo domain.Repo) (domain.Repo, error) {
	repo, err := s.resolveRepoRegistration(ctx, actorID, repo)
	if err != nil {
		return domain.Repo{}, err
	}
	return s.meta.PutRepo(ctx, repo)
}

// Resolving identity/authorization does not register or infer a new repository.
func (s *Service) resolveRepoRegistration(ctx context.Context, actorID string, repo domain.Repo) (domain.Repo, error) {
	var err error
	repo, err = s.resolveRepoRegistrationIdentity(ctx, actorID, repo)
	if err != nil {
		return domain.Repo{}, err
	}
	// Git origin verification (onboarding safety measure): If another team member has already connected to the same cxthub URL (same RepoID), and the git origin of the code repo is confirmed, the git origin of the newly connecting local folder must also be the same. If different, it is rejected — "A folder not connected to git in that repository" is prevented from being attached to the same cxthub URL (same origin: URL is the destination, origin is the substance).
	// The first connector confirms the origin (empty → filled), and thereafter, that value holds authority.
	existing, err := s.meta.GetRepo(ctx, repo.ID)
	if err != nil && !errors.Is(err, domain.ErrNotFound) {
		return domain.Repo{}, err
	}
	if err == nil && existing.GitRemoteURL != "" {
		if normalizeGitURL(repo.GitRemoteURL) != normalizeGitURL(existing.GitRemoteURL) {
			return domain.Repo{}, fmt.Errorf(
				"%w: The git origin (%s) of this folder is different from the git (%s) connected to this repository — try again in the corresponding code repo",
				domain.ErrGitOriginMismatch, gitOriginLabel(repo.GitRemoteURL), gitOriginLabel(existing.GitRemoteURL))
		}
	}
	return repo, nil
}

func (s *Service) resolveRepoRegistrationIdentity(ctx context.Context, actorID string, repo domain.Repo) (domain.Repo, error) {
	if err := domain.ValidateContentHash(repo.ID); err != nil {
		return domain.Repo{}, err
	}
	if repo.DefaultBranch != "" {
		if err := domain.ValidateBranchName(repo.DefaultBranch); err != nil {
			return domain.Repo{}, err
		}
	}
	// Binding is always interpreted by the server from the remote URL path (/<owner_username>/<repository-slug>/…). The repository_id in the body is untrusted — assuming RepoID from URL hash, allowing client-specified bindings can lead to arbitrary repository takeover (repo squatting) if a matching repository is not found (fail-closed).
	repo.RemoteURL = domain.SanitizeRemoteURL(repo.RemoteURL)
	repo.GitRemoteURL = domain.SanitizeRemoteURL(repo.GitRemoteURL)
	repositoryID, err := s.repositoryForURL(ctx, repo.RemoteURL)
	if err != nil {
		return domain.Repo{}, err
	}
	repo.RepositoryID = repositoryID

	// The authenticity of RepoID is not the Git origin but the cxthub destination URL. Connecting the same Git repo to multiple users' repositories results in different RemoteURLs, thus different RepoIDs.
	expectedID := domain.HashContent([]byte(normalizeGitURL(repo.RemoteURL)))
	if repo.ID != expectedID {
		return domain.Repo{}, fmt.Errorf("%w: repo id does not match cxthub remote URL", domain.ErrIntegrity)
	}
	if _, transactional := s.meta.(outbound.RepositoryTransactions); transactional {
		locker, ok := s.repositories.(outbound.RepositoryAccessLocker)
		if !ok {
			return domain.Repo{}, domain.ErrForbidden
		}
		if err := locker.LockRepositoryAccess(ctx, repositoryID, actorID); err != nil {
			return domain.Repo{}, err
		}
		current, err := s.repositoryForURL(ctx, repo.RemoteURL)
		if err != nil {
			return domain.Repo{}, err
		}
		if current != repositoryID {
			return domain.Repo{}, domain.ErrConflict
		}
	}

	// Registration itself creates the state of the target repository. To prevent URL takeover by others before login, context write permissions (member level or above) are required for both initial registration and re-registration.
	record, err := s.repositories.GetRepository(ctx, repositoryID)
	if err != nil {
		return domain.Repo{}, err
	}
	if record.Archived {
		return domain.Repo{}, domain.ErrForbidden
	}
	role, ok := s.repositoryRole(ctx, repositoryID, actorID)
	if !ok || !role.AtLeast(domain.RoleMember) {
		return domain.Repo{}, domain.ErrForbidden
	}

	return repo, nil
}

func (s *Service) repositoryRole(ctx context.Context, repositoryID, userID string) (domain.MemberRole, bool) {
	return repositoryRole(ctx, s.repositories, repositoryID, userID)
}

func normalizeGitURL(raw string) string {
	return domain.NormalizeGitRemoteURL(raw)
}

// gitOriginLabel is a label for validation failure messages (empty means "(none)").
func gitOriginLabel(raw string) string {
	if strings.TrimSpace(raw) == "" {
		return "(none)"
	}
	return raw
}

// GetMemoryObject reads one immutable attachment object by its own hash. Pull
// uses this to prove ancestry before moving a local memory ref.
func (s *Service) GetMemoryObject(ctx context.Context, repoID, hash domain.ContentHash) (domain.MemoryDigest, error) {
	queries, err := s.MemorySnapshotQueries()
	if err != nil {
		return domain.MemoryDigest{}, err
	}
	return queries.GetMemoryObject(ctx, repoID, hash)
}

func settingsKindOK(kind string) bool { return domain.ValidSettingsKind(kind) }

// PutPending upserts the session-specific context pointer (CLI hook capture mirror). The sessionID is authoritative — the body's session/repo is overwritten (push's RepoID normalization equivalent).
// The previous hook leaf is collected only if the replacement verifiably contains its complete event prefix.
func (s *Service) putPending(ctx context.Context, repoID domain.ContentHash, sessionID string, p domain.Pending) error {
	user, err := s.authorizePendingWrite(ctx, repoID, sessionID)
	if err != nil {
		return err
	}
	if user.ID != "" {
		name := user.Name
		if name == "" {
			name = user.Username
		}
		p.Author = domain.TeamIdentity{Name: name, Email: user.Email}
	}
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
	snap, gerr := s.meta.GetSnapshot(ctx, repoID, p.Target)
	if gerr != nil {
		if errors.Is(gerr, domain.ErrNotFound) {
			return fmt.Errorf("%w: pending.target %s snapshot not found on server — object push prerequisite", domain.ErrValidation, p.Target)
		}
		return gerr
	}
	// Display capture time, not replay time. Legacy snapshots can lack a time.
	if !snap.CreatedAt.IsZero() {
		p.UpdatedAt = snap.CreatedAt
	}
	// Future client clocks cannot manufacture indefinite live state.
	if p.ActivityAt != nil && (p.ActivityAt.IsZero() || p.ActivityAt.After(time.Now().UTC().Add(5*time.Second))) {
		p.ActivityAt = nil
	}
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
	return repositoryWriteError(pendingWriteContext(ctx), s, repoID, func(ctx context.Context) error { return s.dismissPending(ctx, repoID, sessionID) })
}
func (s *Service) dismissPending(ctx context.Context, repoID domain.ContentHash, sessionID string) error {
	if _, err := s.authorizePendingWrite(ctx, repoID, sessionID); err != nil {
		return err
	}
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
	return repositoryWriteError(pendingWriteContext(ctx), s, repoID, func(ctx context.Context) error { return s.undismissPending(ctx, repoID, sessionID) })
}
func (s *Service) undismissPending(ctx context.Context, repoID domain.ContentHash, sessionID string) error {
	if _, err := s.authorizePendingWrite(ctx, repoID, sessionID); err != nil {
		return err
	}
	if err := domain.ValidateContentHash(repoID); err != nil {
		return err
	}
	_, err := s.meta.SetPendingDismissed(ctx, repoID, sessionID, false)
	return err
}

// ListPendings returns the durable uncommitted capture pointers for the repo.
func (s *Service) ListPendings(ctx context.Context, repoID domain.ContentHash) ([]domain.Pending, error) {
	return repositoryReadForRepo(ctx, s, repoID, func(ctx context.Context) ([]domain.Pending, error) {
		if err := domain.ValidateContentHash(repoID); err != nil {
			return nil, err
		}
		items, err := s.meta.ListPendings(ctx, repoID)
		if err != nil {
			return nil, err
		}
		// Repair the display of legacy pointers whose timestamp was overwritten on
		// every sync. Immutable snapshot creation remains the capture-time fallback.
		for i := range items {
			if snap, err := s.meta.GetSnapshot(ctx, repoID, items[i].Target); err == nil && !snap.CreatedAt.IsZero() {
				items[i].UpdatedAt = snap.CreatedAt
			}
		}
		return items, nil
	})
}

// DeletePending is the legacy unconditional commit-resolution path. Current
// clients use CompareAndDeletePending so a delayed helper cannot erase a newer
// capture. It is idempotent.
// Releasing a pointer supplies no verified successor, so its capture is retained.
func (s *Service) DeletePending(ctx context.Context, repoID domain.ContentHash, sessionID string) error {
	return repositoryWriteError(pendingWriteContext(ctx), s, repoID, func(ctx context.Context) error { return s.deletePending(ctx, repoID, sessionID) })
}
func (s *Service) deletePending(ctx context.Context, repoID domain.ContentHash, sessionID string) error {
	if _, err := s.authorizePendingWrite(ctx, repoID, sessionID); err != nil {
		return err
	}
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
// This pointer-only operation retains capture data; it supplies no verified
// successor for GC. A concurrent newer pointer returns false and remains untouched.
func (s *Service) CompareAndDeletePending(ctx context.Context, repoID domain.ContentHash, sessionID string, expected domain.ContentHash) (bool, error) {
	return repositoryWrite(pendingWriteContext(ctx), s, repoID, func(ctx context.Context) (bool, error) {
		return s.compareAndDeletePending(ctx, repoID, sessionID, expected)
	})
}
func (s *Service) compareAndDeletePending(ctx context.Context, repoID domain.ContentHash, sessionID string, expected domain.ContentHash) (bool, error) {
	if _, err := s.authorizePendingWrite(ctx, repoID, sessionID); err != nil {
		return false, err
	}
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

// reconcileSharedPendingPointers resolves mutable pending metadata inside the
// repository transaction after branch/join ref movement. A pending target is resolved only when a
// branch, server-managed session, or immutable branch-lifecycle root reaches
// it through natural or graft parents. Compare-and-delete protects a newer
// capture that arrives during the reachability walk.
func (s *Service) reconcileSharedPendingPointers(ctx context.Context, repoID domain.ContentHash) error {
	return s.repositoryRefs().reconcileSharedPendingPointers(ctx, repoID)
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

// gcHookLeaf removes an unreferenced hook leaf only when a verified successor
// contains its complete event prefix. Pointer replacement order does not prove
// supersession: an offline client can publish an older or divergent capture.
// Branch and cwd are not identity; the same native session can move worktrees.
func (s *Service) gcHookLeaf(ctx context.Context, repoID domain.ContentHash, old, current domain.ContentHash) {
	if old == "" || current == "" || old == current {
		return
	}
	snap, err := s.meta.GetSnapshot(ctx, repoID, old)
	if err != nil || !strings.HasPrefix(snap.Message, domain.HookMessagePrefix) {
		return
	}
	archives, err := s.sessionArchiveRecords(ctx, repoID)
	if err != nil {
		return
	}
	for _, archive := range archives {
		if archive.SnapshotID == old || archive.Key == domain.SessionArchiveKey(snap) {
			return
		}
	}
	// A repeated event prefix does not carry the separately authored memory.
	// An attached digest makes this an archive, even without a current ref.
	if snap.MemoryHash != "" {
		return
	}
	// Old installations also have snapshot-keyed memory metadata.
	if _, err := s.meta.GetMemoryMeta(ctx, repoID, old); !errors.Is(err, domain.ErrNotFound) {
		return
	}
	replacement, err := s.meta.GetSnapshot(ctx, repoID, current)
	if err != nil || snap.Provider == "" || snap.SessionID == "" ||
		replacement.Provider != snap.Provider || replacement.SessionID != snap.SessionID {
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
		for _, parent := range sn.ReachabilityParents() {
			if parent == old {
				return // even an unreferenced child still needs this object
			}
		}
		if sn.ID != old && sn.DocHash == snap.DocHash {
			return // another snapshot still owns the document
		}
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
	// Only an unreferenced leaf reaches transcript verification. Keep all guards
	// and deletion within this same repository transaction.
	if !s.captureSupersedes(ctx, repoID, snap, replacement) {
		return
	}
	if err := s.meta.DeleteSnapshot(ctx, repoID, old); err != nil {
		return
	}
	_ = s.blobs.DeleteDoc(ctx, repoID, snap.DocHash)
}

func (s *Service) captureSupersedes(ctx context.Context, repoID domain.ContentHash, snap, replacement domain.Snapshot) bool {
	if comparator, ok := s.blobs.(outbound.StoredCaptureComparator); ok {
		supersedes, err := comparator.CaptureSupersedes(ctx, repoID, snap.DocHash, replacement.DocHash, snap.Provider, snap.SessionID)
		return err == nil && supersedes
	}
	oldDoc, err := s.blobs.GetDoc(ctx, repoID, snap.DocHash)
	if err != nil {
		return false
	}
	newDoc, err := s.blobs.GetDoc(ctx, repoID, replacement.DocHash)
	if err != nil || oldDoc.CIR.Envelope.SourceProvider != snap.Provider ||
		newDoc.CIR.Envelope.SourceProvider != snap.Provider ||
		oldDoc.CIR.Envelope.SessionOriginID != snap.SessionID ||
		newDoc.CIR.Envelope.SessionOriginID != snap.SessionID ||
		len(newDoc.CIR.Events) < len(oldDoc.CIR.Events) {
		return false
	}
	for i, event := range oldDoc.CIR.Events {
		if !reflect.DeepEqual(event, newDoc.CIR.Events[i]) {
			return false
		}
	}
	return true
}

// PutUnsync updates (user, branch) push wait pointers (shadow sync mirror).
// user/branch are authoritative — overwrites body value. If ref is already target,
// it's effectively synced, so instead of upsert, it resolves (deletes).
func (s *Service) putUnsync(ctx context.Context, repoID domain.ContentHash, user, branch string, u domain.Unsync) error {
	if err := s.authorizeUnsyncWrite(ctx, user); err != nil {
		return err
	}
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
			return s.resolveUnsync(ctx, repoID, user, branch)
		}
	}
	u.RepoID = repoID
	u.User = user
	u.Branch = branch
	u.UpdatedAt = time.Now().UTC()
	if err := s.meta.PutUnsync(ctx, repoID, u); err != nil {
		return err
	}
	return revisionWrite(context.WithValue(ctx, revisionScopeKey{}, "graph"), s, repoID)
}

// ListUnsyncs returns the entire push wait pointer set for the repo (for web On Hold display).
func (s *Service) ListUnsyncs(ctx context.Context, repoID domain.ContentHash) ([]domain.Unsync, error) {
	return repositoryReadForRepo(ctx, s, repoID, func(ctx context.Context) ([]domain.Unsync, error) {
		if err := domain.ValidateContentHash(repoID); err != nil {
			return nil, err
		}
		return s.meta.ListUnsyncs(ctx, repoID)
	})
}

// DeleteUnsync resolves a push wait pointer (git push/manual cleanup — idempotent).
func (s *Service) DeleteUnsync(ctx context.Context, repoID domain.ContentHash, user, branch string) error {
	ctx = context.WithValue(ctx, revisionScopeKey{}, "none")
	return repositoryWriteError(ctx, s, repoID, func(ctx context.Context) error { return s.deleteUnsync(ctx, repoID, user, branch) })
}
func (s *Service) deleteUnsync(ctx context.Context, repoID domain.ContentHash, user, branch string) error {
	if err := s.authorizeUnsyncWrite(ctx, user); err != nil {
		return err
	}
	if err := domain.ValidateContentHash(repoID); err != nil {
		return err
	}
	if err := domain.ValidateBranchName(branch); err != nil {
		return err
	}
	return s.resolveUnsync(ctx, repoID, user, branch)
}

func (s *Service) resolveUnsync(ctx context.Context, repoID domain.ContentHash, user, branch string) error {
	if storage, ok := s.meta.(outbound.UnsyncDeletionStore); ok {
		changed, err := storage.DeleteUnsyncIfPresent(ctx, repoID, user, branch)
		if err != nil || !changed {
			return err
		}
	} else if err := s.meta.DeleteUnsync(ctx, repoID, user, branch); err != nil {
		return err
	}
	return revisionWrite(context.WithValue(ctx, revisionScopeKey{}, "graph"), s, repoID)
}

// GetSecrets returns secret ciphertext envelopes (ciphertext as-is — decryption is on the client).
func (s *Service) GetSecrets(ctx context.Context, repoID domain.ContentHash) ([]byte, error) {
	return repositoryReadForRepo(ctx, s, repoID, func(ctx context.Context) ([]byte, error) {
		if err := domain.ValidateContentHash(repoID); err != nil {
			return nil, err
		}
		return s.meta.GetSecretsEnvelope(ctx, repoID)
	})
}

// GetSettingsObjectByHash retrieves a commit attachment settings object by hash.
func (s *Service) GetSettingsObjectByHash(ctx context.Context, repoID domain.ContentHash, hash domain.ContentHash) (domain.SettingsBundle, error) {
	return repositoryReadForRepo(ctx, s, repoID, func(ctx context.Context) (domain.SettingsBundle, error) {
		if err := validateHashes(repoID, hash); err != nil {
			return domain.SettingsBundle{}, err
		}
		return s.meta.GetSettingsObject(ctx, repoID, hash)
	})
}

// GetSettings retrieves setting bundles.
func (s *Service) GetSettings(ctx context.Context, repoID domain.ContentHash, kind string) (domain.SettingsBundle, error) {
	return repositoryReadForRepo(ctx, s, repoID, func(ctx context.Context) (domain.SettingsBundle, error) {
		if err := domain.ValidateContentHash(repoID); err != nil {
			return domain.SettingsBundle{}, err
		}
		if !settingsKindOK(kind) {
			return domain.SettingsBundle{}, domain.ErrNotFound
		}
		return s.meta.GetSettingsBundle(ctx, repoID, kind)
	})
}

// pathSegs splits a URL path into a slice of non-empty segment strings.
func pathSegs(p string) []string {
	p = strings.Trim(p, "/")
	if p == "" {
		return nil
	}
	return strings.Split(p, "/")
}

// repositoryForURL resolves /<owner>/<repository> or an exact historical
// three-segment alias. Aliases retain the original content identity and always
// authorize against their current repository boundary.
func (s *Service) repositoryForURL(ctx context.Context, remoteURL string) (string, error) {
	if s.repositories == nil {
		return "", fmt.Errorf("%w: repository binding store unavailable", domain.ErrForbidden)
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
	if len(seg) != 2 && len(seg) != 3 {
		return "", fmt.Errorf("%w: repo URL must be <host>/<owner>/<repository> (or an existing legacy connection alias): %q", domain.ErrValidation, remoteURL)
	}
	for _, part := range seg {
		if part == "" || part == "." || part == ".." {
			return "", fmt.Errorf("%w: repo URL contains an invalid path segment: %q", domain.ErrValidation, remoteURL)
		}
	}
	var repositoryRecord domain.Repository
	if namespaces, ok := s.repositories.(outbound.OrganizationStore); ok {
		if namespace, nerr := namespaces.GetNamespaceBySlug(ctx, seg[0]); nerr == nil {
			repositoryRecord, err = s.repositories.GetRepositoryByNamespacePath(ctx, namespace.ID, strings.Join(seg[1:], "/"))
			if errors.Is(err, domain.ErrNotFound) {
				repositoryRecord, err = s.repositories.GetRepositoryByPath(ctx, seg[0], strings.Join(seg[1:], "/"))
			}
		} else {
			if !errors.Is(nerr, domain.ErrNotFound) {
				return "", nerr
			}
			// The exact stored alias may use a handle predating the registry.
			repositoryRecord, err = s.repositories.GetRepositoryByPath(ctx, seg[0], strings.Join(seg[1:], "/"))
		}
	} else {
		repositoryRecord, err = s.repositories.GetRepositoryByPath(ctx, seg[0], strings.Join(seg[1:], "/"))
	}
	if err != nil {
		return "", fmt.Errorf("%w: repo remote_url does not match an existing repository: %q", domain.ErrForbidden, remoteURL)
	}
	return repositoryRecord.ID, nil
}

func (s *Service) ListRepos(ctx context.Context, team string) ([]domain.Repo, error) {
	return repositoryRead(ctx, s, func(ctx context.Context) ([]domain.Repo, error) {
		repos, err := s.meta.ListRepos(ctx, team)
		if err != nil {
			return nil, err
		}
		for _, r := range repos {
			if err := r.RequiredDocIdentity.Validate(); err != nil {
				return nil, err
			}
		}
		return repos, nil
	})
}

// Activity groups the "Contribution activity" feed by month (latest month first):
// Monthly context commit bundles (repository counts) + repositories created that month.
// Repositories are already filtered by visibility in the caller (PublicUser).
func (s *Service) Activity(ctx context.Context, repositories []domain.Repository) ([]domain.ActivityMonth, error) {
	ctx = outbound.WithDocumentIdentityCompatibility(ctx, inbound.DocumentIdentities(ctx), s.DocumentIdentitiesSupported())
	return repositoryRead(ctx, s, func(ctx context.Context) ([]domain.ActivityMonth, error) { return s.activity(ctx, repositories) })
}
func (s *Service) activity(ctx context.Context, repositories []domain.Repository) ([]domain.ActivityMonth, error) {
	repositoryByID := make(map[string]domain.Repository, len(repositories))
	repositoryIDs := make([]string, 0, len(repositories))
	for _, w := range repositories {
		repositoryByID[w.ID] = w
		repositoryIDs = append(repositoryIDs, w.ID)
	}
	days, err := s.commitActivityDays(ctx, repositoryIDs)
	if err != nil {
		return nil, err
	}
	commits := map[string]map[string]int{} // month -> repositoryID -> count
	for repositoryID, counts := range days {
		for date, count := range counts {
			month := date[:7]
			if commits[month] == nil {
				commits[month] = map[string]int{}
			}
			commits[month][repositoryID] += count
		}
	}
	created := map[string][]domain.Repository{} // month -> repositories created
	for _, w := range repositories {
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

	path := func(w domain.Repository) string { return w.OwnerUsername + "/" + w.Slug }
	out := make([]domain.ActivityMonth, 0, len(months))
	for _, m := range months {
		// These fields are required arrays in the public API. A nil Go slice is
		// encoded as JSON null, which makes otherwise valid commit-only or
		// creation-only months unsafe for clients to consume as collections.
		am := domain.ActivityMonth{
			Month:       m,
			CommitRepos: make([]domain.ActivityRepo, 0),
			Created:     make([]domain.ActivityCreated, 0),
		}
		for repositoryID, c := range commits[m] {
			w := repositoryByID[repositoryID]
			am.CommitTotal += c
			am.CommitRepos = append(am.CommitRepos, domain.ActivityRepo{Name: w.Name, Path: path(w), Count: c})
		}
		sort.Slice(am.CommitRepos, func(i, j int) bool { return am.CommitRepos[i].Count > am.CommitRepos[j].Count })
		createdRepository := created[m]
		sort.Slice(createdRepository, func(i, j int) bool { return createdRepository[i].CreatedAt.After(createdRepository[j].CreatedAt) })
		for _, w := range createdRepository {
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

// Contributions aggregates finalized commit-linked history by recorded event
// date (YYYY-MM-DD, UTC) for caller-filtered repositories, without author claims.
func (s *Service) Contributions(ctx context.Context, repositoryIDs []string) (map[string]int, error) {
	ctx = outbound.WithDocumentIdentityCompatibility(ctx, inbound.DocumentIdentities(ctx), s.DocumentIdentitiesSupported())
	return repositoryRead(ctx, s, func(ctx context.Context) (map[string]int, error) { return s.contributions(ctx, repositoryIDs) })
}
func (s *Service) contributions(ctx context.Context, repositoryIDs []string) (map[string]int, error) {
	out := map[string]int{}
	days, err := s.commitActivityDays(ctx, repositoryIDs)
	if err != nil {
		return nil, err
	}
	for _, counts := range days {
		for date, count := range counts {
			out[date] += count
		}
	}
	return out, nil
}

func (s *Service) commitActivityDays(ctx context.Context, repositoryIDs []string) (map[string]map[string]int, error) {
	out := map[string]map[string]int{}
	if len(repositoryIDs) == 0 {
		return out, nil
	}
	wanted := make(map[string]bool, len(repositoryIDs))
	for _, id := range repositoryIDs {
		wanted[id] = true
	}
	repos, err := s.meta.ListRepos(ctx, "default")
	if err != nil {
		return nil, err
	}
	historyStore, supported := s.meta.(outbound.HistoryStore)
	seen := make(map[domain.ContentHash]bool)
	for _, r := range repos {
		if !wanted[r.RepositoryID] || seen[r.ID] {
			continue
		}
		seen[r.ID] = true
		if err := outbound.CheckDocumentIdentityCompatibility(ctx, r.RequiredDocIdentity); err != nil {
			if errors.Is(err, domain.ErrDocumentIdentityUpgradeRequired) {
				continue
			}
			return nil, err
		}
		if !supported {
			return nil, fmt.Errorf("context history storage unavailable")
		}
		history, err := historyStore.ListHistoryEvents(ctx, r.ID)
		if err != nil {
			return nil, err
		}
		days, err := domain.CommitActivityDays(r.ID, history)
		if err != nil {
			return nil, err
		}
		if out[r.RepositoryID] == nil {
			out[r.RepositoryID] = map[string]int{}
		}
		for date, count := range days {
			out[r.RepositoryID][date] += count
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
	return repositoryReadForRepo(ctx, s, repoID, func(ctx context.Context) (domain.Snapshot, error) {
		if err := validateHashes(repoID, id); err != nil {
			return domain.Snapshot{}, err
		}
		return s.meta.GetSnapshot(ctx, repoID, id)
	})
}
func (s *Service) getDoc(ctx context.Context, repoID, hash domain.ContentHash) (domain.SessionDoc, error) {
	if err := validateHashes(repoID, hash); err != nil {
		return domain.SessionDoc{}, err
	}
	return s.materializedDocumentRead(ctx, repoID, hash)
}
func (s *Service) listRefs(ctx context.Context, repoID domain.ContentHash) ([]domain.Ref, error) {
	if err := domain.ValidateContentHash(repoID); err != nil {
		return nil, err
	}
	return s.meta.ListRefs(ctx, repoID)
}

// Fork creates a new branch ref with tip FromSnapshot (O(1), original unchanged F1).
func (s *Service) fork(ctx context.Context, in inbound.ForkInput) (inbound.ForkOutput, error) {
	commands := s.branchOperations()
	if err := commands.ready(); err != nil {
		return inbound.ForkOutput{}, err
	}
	return commands.fork(ctx, in)
}

// join loads the repository graph inside the write boundary and executes the
// domain plan. Ref allocation and persistence stay in the application/adapters.
func (s *Service) join(ctx context.Context, in inbound.JoinInput) (inbound.JoinOutput, error) {
	commands := s.branchOperations()
	if err := commands.ready(); err != nil {
		return inbound.JoinOutput{}, err
	}
	return commands.join(ctx, in)
}

func (s *Service) loadJoinGraph(ctx context.Context, repo domain.ContentHash) (domain.JoinGraph, error) {
	commands := s.branchOperations()
	if err := commands.ready(); err != nil {
		return domain.JoinGraph{}, err
	}
	return commands.loadJoinGraph(ctx, repo)
}

// joinForkRefName selects a session ref name to preserve natural descendants after partial join. The session ref itself is not a real git branch membership, but the branch scope in the name participates in join ownership determination. Actual creation and conflict final determination is performed in the ApplyJoin atomic boundary.
func (s *Service) joinForkRefName(ctx context.Context, repoID domain.ContentHash, branch string, tip domain.ContentHash) (string, error) {
	commands := s.branchOperations()
	if err := commands.ready(); err != nil {
		return "", err
	}
	return commands.joinForkRefName(ctx, repoID, branch, tip)
}

// Diff calculates the CIR event delta (add/remove) between two snapshots based on content keys.
func (s *Service) Diff(ctx context.Context, in inbound.DiffInput) (inbound.DiffOutput, error) {
	if err := validateHashes(in.RepoID, in.Left, in.Right); err != nil {
		return inbound.DiffOutput{}, err
	}
	return resourceReadForRepo(ctx, s, in.RepoID, func(bound context.Context) (inbound.DiffOutput, error) {
		left, err := s.getDoc(bound, in.RepoID, in.Left)
		if err != nil {
			return inbound.DiffOutput{}, err
		}
		right := left
		if in.Right != in.Left {
			right, err = s.getDoc(bound, in.RepoID, in.Right)
			if err != nil {
				return inbound.DiffOutput{}, err
			}
		}
		return inbound.DiffOutput{Changes: diffEvents(left.CIR.Events, right.CIR.Events)}, nil
	})
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
