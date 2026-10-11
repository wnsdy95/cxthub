package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type prContributionMemoryState string

const (
	prMemorySelected         prContributionMemoryState = "selected"
	prMemorySuperseded       prContributionMemoryState = "superseded"
	prMemoryEmpty            prContributionMemoryState = "empty"
	prMemoryUnavailable      prContributionMemoryState = "unavailable"
	prMemoryReview           prContributionMemoryState = "review"
	maxPRMemoryMetadataBytes                           = 4 << 20
)

type prContributionMemoryVersion struct {
	Source         domain.ContentHash           `json:"source_snapshot"`
	Hash           domain.ContentHash           `json:"memory_hash"`
	Pins           []contributionMemoryPin      `json:"pins"`
	State          prContributionMemoryState    `json:"state"`
	ProjectionHash domain.ContentHash           `json:"projection_hash,omitempty"`
	Independent    bool                         `json:"independent,omitempty"`
	MissingPins    []prContributionMemoryPinKey `json:"missing_pins,omitempty"`
	digest         domain.MemoryDigest
}

type prContributionMemoryPinKey struct {
	ContributionID string `json:"contribution_id"`
	PublicationID  string `json:"publication_id"`
}

type prContributionMemoryPlan struct {
	Snapshots []domain.Snapshot
	Versions  []prContributionMemoryVersion
	excluded  map[domain.ContentHash]bool
	order     map[domain.ContentHash]int
}

func (s *Service) preparePRContributionMemory(ctx context.Context, repo domain.ContentHash, inclusion domain.BranchContext, snapshots []domain.Snapshot, verified []verifiedPRContribution) (prContributionMemoryPlan, error) {
	var pins []domain.PRContributionMemory
	if len(verified) > 0 {
		ledger, ok := s.meta.(outbound.PRContributionStore)
		if !ok {
			return prContributionMemoryPlan{}, domain.ErrIntegrity
		}
		var err error
		pins, err = ledger.ListPRContributionMemories(ctx, repo)
		if err != nil {
			return prContributionMemoryPlan{}, err
		}
	}
	return s.prepareVerifiedPRContributionMemory(ctx, repo, inclusion, snapshots, verified, pins)
}

func (s *Service) prepareVerifiedContributionMemory(ctx context.Context, repo domain.ContentHash, inclusion domain.BranchContext, snapshots []domain.Snapshot, verified []verifiedMemoryContribution, pins []contributionMemoryPin) (prContributionMemoryPlan, error) {
	plan := prContributionMemoryPlan{Snapshots: append([]domain.Snapshot{}, snapshots...), excluded: map[domain.ContentHash]bool{}, order: map[domain.ContentHash]int{}}
	if err := ctx.Err(); err != nil {
		return plan, err
	}
	if len(verified) == 0 {
		return plan, nil
	}
	if len(verified) > maxProjectionSnapshots || len(pins) > maxProjectionSnapshots {
		return plan, domain.ErrMemoryProjectionLimit
	}
	byID := map[domain.ContentHash]domain.Snapshot{}
	allowed := map[domain.ContentHash]bool{}
	for index, id := range inclusion.SnapshotIDs {
		allowed[id] = true
		plan.order[id] = index
	}
	for _, snapshot := range snapshots {
		if snapshot.RepoID != repo {
			return plan, domain.ErrIntegrity
		}
		if _, duplicate := byID[snapshot.ID]; duplicate {
			return plan, domain.ErrIntegrity
		}
		byID[snapshot.ID] = snapshot
	}
	contributions := map[string]verifiedMemoryContribution{}
	groups := map[string][]verifiedMemoryContribution{}
	managed := map[domain.ContentHash]bool{}
	for _, contribution := range verified {
		if contribution.record.RepoID != repo {
			return plan, domain.ErrIntegrity
		}
		contributions[contribution.record.ID] = contribution
		key := contributionMergeKey(contribution.record.DestinationBranchID, contribution.record.Capture, contribution.record.IntegrationSHA)
		groups[key] = append(groups[key], contribution)
		managed[contribution.record.Capture] = true
	}
	eligible := map[string]verifiedMemoryContribution{}
	ordinaryRoots := []domain.ContentHash{inclusion.SnapshotID}
	for _, root := range inclusion.Roots {
		if !managed[root] {
			ordinaryRoots = append(ordinaryRoots, root)
		}
	}
	for _, merge := range inclusion.Merges {
		key := contributionMergeKey(merge.DestinationBranchID, merge.Source, merge.MergeSHA)
		contribution, exists := contributions[merge.EventID]
		if len(groups[key]) == 0 && merge.State == "included" {
			ordinaryRoots = append(ordinaryRoots, merge.Source)
		}
		if exists && (merge.Source != contribution.record.Capture || merge.DestinationBranchID != contribution.record.DestinationBranchID || merge.MergeSHA != contribution.record.IntegrationSHA) {
			return plan, domain.ErrIntegrity
		}
		if merge.State == "included" {
			if !allowed[merge.Source] {
				return plan, domain.ErrIntegrity
			}
			for _, candidate := range groups[key] {
				if candidate.valid {
					eligible[candidate.record.ID] = candidate
				}
			}
		}
	}
	ordinary := map[domain.ContentHash]bool{}
	edges := 0
	for len(ordinaryRoots) > 0 {
		if err := ctx.Err(); err != nil {
			return plan, err
		}
		id := ordinaryRoots[len(ordinaryRoots)-1]
		ordinaryRoots = ordinaryRoots[:len(ordinaryRoots)-1]
		if ordinary[id] || !allowed[id] {
			continue
		}
		snapshot, exists := byID[id]
		if !exists {
			return plan, domain.ErrIntegrity
		}
		ordinary[id] = true
		edges += len(snapshot.Parents) + len(snapshot.GraftParents)
		if len(ordinary) > maxProjectionSnapshots || edges > maxProjectionEdges {
			return plan, domain.ErrMemoryProjectionLimit
		}
		ordinaryRoots = append(ordinaryRoots, snapshot.Parents...)
		ordinaryRoots = append(ordinaryRoots, snapshot.GraftParents...)
	}
	for index := range plan.Snapshots {
		id := plan.Snapshots[index].ID
		if allowed[id] && !ordinary[id] {
			plan.Snapshots[index].MemoryHash = ""
			plan.excluded[id] = true
		}
	}
	type versionKey struct{ source, hash domain.ContentHash }
	versions := map[versionKey]*prContributionMemoryVersion{}
	seen := map[string]bool{}
	acceptedPins := map[prContributionMemoryPinKey]bool{}
	for _, pin := range pins {
		if err := ctx.Err(); err != nil {
			return plan, err
		}
		if pin.RepoID != repo {
			return plan, domain.ErrIntegrity
		}
		contribution, exists := eligible[pin.ContributionID]
		if !exists || seen[pin.ID] {
			continue
		}
		publication, exists := contribution.publications[pin.PublicationID]
		if !exists {
			continue
		}
		memorySource := publication.MemorySource
		if memorySource == "" && publication.MemoryHash != "" {
			memorySource = contribution.record.Capture
		}
		if publication.Kind != "publish" || publication.RepoID != string(repo) || publication.BranchID != contribution.record.SourceBranchID || publication.Target != contribution.record.Capture || publication.Source != publication.Target || publication.GitAfter != contribution.record.OriginalHead || publication.MemoryHash != pin.MemoryHash || memorySource != pin.MemorySource || publication.MemoryPinned != pin.MemoryPinned {
			return plan, domain.ErrIntegrity
		}
		seen[pin.ID] = true
		acceptedPins[prContributionMemoryPinKey{pin.ContributionID, pin.PublicationID}] = true
		source := pin.MemorySource
		if source == "" {
			source = contribution.record.Capture
		}
		key := versionKey{source, pin.MemoryHash}
		version := versions[key]
		if version == nil {
			version = &prContributionMemoryVersion{Source: source, Hash: pin.MemoryHash, State: prMemorySelected}
			versions[key] = version
		}
		version.Pins = append(version.Pins, pin)
	}
	publicationCount := 0
	for _, contribution := range eligible {
		for _, publication := range contribution.publications {
			publicationCount++
			if publicationCount > maxProjectionSnapshots {
				return plan, domain.ErrMemoryProjectionLimit
			}
			pinKey := prContributionMemoryPinKey{contribution.record.ID, publication.ID}
			if acceptedPins[pinKey] {
				continue
			}
			source := publication.MemorySource
			if source == "" {
				source = contribution.record.Capture
			}
			key := versionKey{source, publication.MemoryHash}
			version := versions[key]
			if version == nil {
				version = &prContributionMemoryVersion{Source: source, Hash: publication.MemoryHash}
				versions[key] = version
			}
			version.State = prMemoryUnavailable
			version.MissingPins = append(version.MissingPins, pinKey)
		}
	}
	for _, version := range versions {
		sort.Slice(version.Pins, func(left, right int) bool { return version.Pins[left].ID < version.Pins[right].ID })
		sort.Slice(version.MissingPins, func(left, right int) bool {
			if version.MissingPins[left].ContributionID != version.MissingPins[right].ContributionID {
				return version.MissingPins[left].ContributionID < version.MissingPins[right].ContributionID
			}
			return version.MissingPins[left].PublicationID < version.MissingPins[right].PublicationID
		})
		plan.Versions = append(plan.Versions, *version)
	}
	sort.Slice(plan.Versions, func(left, right int) bool {
		if plan.Versions[left].Source != plan.Versions[right].Source {
			return plan.Versions[left].Source < plan.Versions[right].Source
		}
		return plan.Versions[left].Hash < plan.Versions[right].Hash
	})
	reader := &projectionReader{source: serviceProjectionSource{s.meta, s.blobs}, repoID: repo}
	chains := map[domain.ContentHash]prMemoryParent{}
	steps := 0
	if err := validatePRMemoryTopology(ctx, byID, plan.Versions); err != nil {
		return plan, err
	}
	for start := 0; start < len(plan.Versions); {
		end := start + 1
		for end < len(plan.Versions) && plan.Versions[end].Source == plan.Versions[start].Source {
			end++
		}
		if err := selectPRMemoryVersions(ctx, reader, byID, plan.Versions[start:end], chains, &steps); err != nil {
			return plan, err
		}
		start = end
	}
	for index := range plan.Versions {
		if ordinary[plan.Versions[index].Source] {
			plan.Versions[index].Independent = true
		}
		version := plan.Versions[index]
		if _, exists := plan.order[version.Source]; !exists {
			for _, pin := range version.Pins {
				rank := plan.order[eligible[pin.ContributionID].record.Capture]
				if previous, known := plan.order[version.Source]; !known || rank > previous {
					plan.order[version.Source] = rank
				}
			}
		}
		for _, fragment := range version.digest.Fragments {
			if _, exists := plan.order[fragment.SourceSnapshot]; !exists {
				plan.order[fragment.SourceSnapshot] = plan.order[version.Source]
			}
		}
	}
	metadata, err := json.Marshal(plan.Versions)
	if err != nil {
		return plan, err
	}
	if len(metadata) > maxPRMemoryMetadataBytes {
		return plan, domain.ErrMemoryProjectionLimit
	}
	return plan, nil
}

type prMemoryParent struct{ source, parent domain.ContentHash }

func validatePRMemoryTopology(ctx context.Context, snapshots map[domain.ContentHash]domain.Snapshot, versions []prContributionMemoryVersion) error {
	states := map[domain.ContentHash]uint8{}
	edges := 0
	var visit func(domain.ContentHash) error
	visit = func(id domain.ContentHash) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if states[id] == 2 {
			return nil
		}
		if states[id] == 1 {
			return domain.ErrIntegrity
		}
		if len(states) >= maxProjectionSnapshots {
			return domain.ErrMemoryProjectionLimit
		}
		snapshot, exists := snapshots[id]
		if !exists {
			return domain.ErrIntegrity
		}
		edges += len(snapshot.Parents)
		if edges > maxProjectionEdges {
			return domain.ErrMemoryProjectionLimit
		}
		states[id] = 1
		for _, parent := range snapshot.Parents {
			if err := visit(parent); err != nil {
				return err
			}
		}
		states[id] = 2
		return nil
	}
	for _, version := range versions {
		if version.Hash != "" && version.State != prMemoryUnavailable {
			if err := visit(version.Source); err != nil {
				return err
			}
		}
	}
	return nil
}

func selectPRMemoryVersions(ctx context.Context, reader *projectionReader, snapshots map[domain.ContentHash]domain.Snapshot, versions []prContributionMemoryVersion, chains map[domain.ContentHash]prMemoryParent, steps *int) error {
	candidates := map[domain.ContentHash]int{}
	uncertain := false
	for index := range versions {
		if versions[index].State == prMemoryUnavailable {
			uncertain = true
			continue
		}
		if versions[index].Hash == "" {
			versions[index].State = prMemoryUnavailable
			for _, pin := range versions[index].Pins {
				if pin.MemoryPinned {
					versions[index].State = prMemoryEmpty
				}
			}
			continue
		}
		candidates[versions[index].Hash] = index
	}
	superseded := map[domain.ContentHash]bool{}
	missing := map[domain.ContentHash]bool{}
	for _, version := range versions {
		if version.State == prMemoryUnavailable {
			continue
		}
		visited := map[domain.ContentHash]bool{}
		for hash := version.Hash; hash != ""; {
			if err := ctx.Err(); err != nil {
				return err
			}
			*steps++
			if *steps > maxProjectionEdges {
				return domain.ErrMemoryProjectionLimit
			}
			if visited[hash] {
				return domain.ErrIntegrity
			}
			visited[hash] = true
			if hash != version.Hash {
				if _, exists := candidates[hash]; exists {
					superseded[hash] = true
				}
			}
			if missing[hash] {
				break
			}
			parent, loaded := chains[hash]
			if !loaded {
				if len(chains) >= maxProjectionSnapshots {
					return domain.ErrMemoryProjectionLimit
				}
				digest, err := reader.GetMemory(ctx, hash)
				if errors.Is(err, domain.ErrNotFound) {
					uncertain = true
					missing[hash] = true
					break
				}
				if err != nil {
					return err
				}
				if reader.bytes > maxProjectionMemoryBytes {
					return domain.ErrMemoryProjectionLimit
				}
				if digest.SnapshotID != version.Source || domain.ValidateOptionalContentHash(digest.PreviousMemoryHash) != nil || digest.ValidateMemoryClaims() != nil {
					return domain.ErrIntegrity
				}
				parent = prMemoryParent{version.Source, digest.PreviousMemoryHash}
				chains[hash] = parent
			}
			if parent.source != version.Source {
				return domain.ErrIntegrity
			}
			hash = parent.parent
		}
	}
	tips := 0
	for index := range versions {
		version := &versions[index]
		if version.State == prMemoryUnavailable {
			continue
		}
		if missing[version.Hash] {
			version.State = prMemoryUnavailable
		} else if superseded[version.Hash] {
			version.State = prMemorySuperseded
		}
		if version.State == prMemorySelected || version.State == prMemoryEmpty {
			tips++
		}
	}
	for index := range versions {
		version := &versions[index]
		if version.State == prMemorySuperseded || version.State == prMemoryUnavailable {
			continue
		}
		if tips > 1 || uncertain {
			version.State = prMemoryReview
		}
		if version.State != prMemorySelected {
			continue
		}
		pinned := prPinnedMemoryStore{reader: reader, snapshots: snapshots, source: version.Source, hash: version.Hash, steps: steps}
		digest, _, complete, err := memoryProjectionFromDetailed(ctx, pinned, version.Source)
		if err != nil {
			return err
		}
		if !complete {
			return domain.ErrIntegrity
		}
		if reader.bytes > maxProjectionMemoryBytes {
			return domain.ErrMemoryProjectionLimit
		}
		version.digest = digest
		version.ProjectionHash, err = domain.MemoryDigestHash(digest)
		if err != nil {
			return err
		}
	}
	return nil
}

type prPinnedMemoryStore struct {
	reader       *projectionReader
	snapshots    map[domain.ContentHash]domain.Snapshot
	source, hash domain.ContentHash
	steps        *int
}

func (store prPinnedMemoryStore) GetSnapshot(ctx context.Context, id domain.ContentHash) (domain.Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return domain.Snapshot{}, err
	}
	*store.steps++
	if *store.steps > maxProjectionEdges {
		return domain.Snapshot{}, domain.ErrMemoryProjectionLimit
	}
	snapshot, exists := store.snapshots[id]
	if !exists {
		return snapshot, domain.ErrIntegrity
	}
	snapshot.GraftParents, snapshot.GraftSeq, snapshot.MemoryHash = nil, 0, ""
	if id == store.source {
		snapshot.MemoryHash = store.hash
	}
	return snapshot, nil
}

func (store prPinnedMemoryStore) GetMemory(ctx context.Context, hash domain.ContentHash) (domain.MemoryDigest, error) {
	return store.reader.GetMemory(ctx, hash)
}

func (plan prContributionMemoryPlan) Merge(ctx context.Context, projection domain.MemoryProjection) (domain.MemoryProjection, error) {
	if err := ctx.Err(); err != nil {
		return domain.MemoryProjection{}, err
	}
	if len(plan.excluded) == 0 && len(plan.Versions) == 0 {
		return projection, nil
	}
	for _, version := range plan.Versions {
		if !version.Independent && (version.State == prMemoryReview || version.State == prMemoryUnavailable) {
			return domain.MemoryProjection{}, errors.Join(domain.ErrConflict, fmt.Errorf("PR contribution memory %s: source=%s hash=%s", version.State, version.Source, version.Hash))
		}
	}
	base := projection.Digest
	if len(base.Fragments) > 0 {
		base.Fragments = append([]domain.MemoryFragment{}, base.Fragments...)
		kept := base.Fragments[:0]
		for _, fragment := range base.Fragments {
			if !plan.excluded[fragment.SourceSnapshot] {
				kept = append(kept, fragment)
			}
		}
		base.Fragments = kept
		base.Summary, base.KeyFacts, base.OpenTasks = "", nil, nil
		base = domain.MergeDigests(domain.MemoryDigest{}, base)
	}
	accumulator := domain.NewMemoryAccumulator(maxProjectionMemoryBytes)
	for _, version := range plan.Versions {
		if err := ctx.Err(); err != nil {
			return domain.MemoryProjection{}, err
		}
		if version.State != prMemorySelected || version.Independent {
			continue
		}
		if err := accumulator.Add(version.digest); err != nil {
			return domain.MemoryProjection{}, err
		}
		projection.Found = true
	}
	if err := accumulator.Add(base); err != nil {
		return domain.MemoryProjection{}, err
	}
	digest, err := accumulator.Digest()
	if err != nil {
		return domain.MemoryProjection{}, err
	}
	if len(digest.Fragments) > 0 {
		rank := func(source domain.ContentHash) int {
			if order, exists := plan.order[source]; exists {
				return order
			}
			return maxProjectionSnapshots
		}
		sort.SliceStable(digest.Fragments, func(left, right int) bool {
			return rank(digest.Fragments[left].SourceSnapshot) > rank(digest.Fragments[right].SourceSnapshot)
		})
		digest = domain.MergeDigests(domain.MemoryDigest{}, digest)
	}
	digest.SnapshotID, digest.PreviousMemoryHash, digest.GraftCoverage = base.SnapshotID, "", nil
	projection.Digest = digest
	raw, err := json.Marshal(struct {
		Version  int
		Base     domain.ContentHash
		Versions []prContributionMemoryVersion
	}{1, projection.StateHash, plan.Versions})
	if err != nil {
		return domain.MemoryProjection{}, err
	}
	if len(raw) > maxPRMemoryMetadataBytes {
		return domain.MemoryProjection{}, domain.ErrMemoryProjectionLimit
	}
	projection.StateHash = domain.HashContent(raw)
	return projection, nil
}
