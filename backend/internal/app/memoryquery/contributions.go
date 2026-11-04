package memoryquery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/wnsdy95/cxthub/backend/internal/app/contextprojection"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type ContributionRecord struct {
	ID                  string
	RepoID              domain.ContentHash
	DestinationBranchID string
	SourceBranchID      string
	Capture             domain.ContentHash
	OriginalHead        string
	IntegrationSHA      string
}

type VerifiedContribution struct {
	Record       ContributionRecord
	Valid        bool
	Publications map[string]domain.HistoryEvent
}

type ContributionPin struct {
	ID             string             `json:"id"`
	RepoID         domain.ContentHash `json:"repo_id"`
	ContributionID string             `json:"contribution_id"`
	PublicationID  string             `json:"publication_id"`
	MemoryHash     domain.ContentHash `json:"memory_hash"`
	MemorySource   domain.ContentHash `json:"memory_source"`
	MemoryPinned   bool               `json:"memory_pinned"`
}

type ContributionState string

const (
	ContributionSelected         ContributionState = "selected"
	ContributionSuperseded       ContributionState = "superseded"
	ContributionEmpty            ContributionState = "empty"
	ContributionUnavailable      ContributionState = "unavailable"
	ContributionReview           ContributionState = "review"
	MaxContributionMetadataBytes                   = 4 << 20
)

type ContributionVersion struct {
	Source         domain.ContentHash   `json:"source_snapshot"`
	Hash           domain.ContentHash   `json:"memory_hash"`
	Pins           []ContributionPin    `json:"pins"`
	State          ContributionState    `json:"state"`
	ProjectionHash domain.ContentHash   `json:"projection_hash,omitempty"`
	Independent    bool                 `json:"independent,omitempty"`
	MissingPins    []ContributionPinKey `json:"missing_pins,omitempty"`
	digest         domain.MemoryDigest
}

type ContributionPinKey struct {
	ContributionID string `json:"contribution_id"`
	PublicationID  string `json:"publication_id"`
}

type ContributionPlan struct {
	Snapshots []domain.Snapshot
	Versions  []ContributionVersion
	excluded  map[domain.ContentHash]bool
	order     map[domain.ContentHash]int
}

func (version ContributionVersion) Digest() domain.MemoryDigest {
	return version.digest
}

func (plan ContributionPlan) Excluded(id domain.ContentHash) bool {
	return plan.excluded[id]
}

func PrepareVerifiedContributionMemory(ctx context.Context, source outbound.MemoryProjectionReader, repo domain.ContentHash, inclusion domain.BranchContext, snapshots []domain.Snapshot, verified []VerifiedContribution, pins []ContributionPin) (ContributionPlan, error) {
	plan := ContributionPlan{Snapshots: append([]domain.Snapshot{}, snapshots...), excluded: map[domain.ContentHash]bool{}, order: map[domain.ContentHash]int{}}
	if err := ctx.Err(); err != nil {
		return plan, err
	}
	if len(verified) == 0 {
		return plan, nil
	}
	if len(verified) > MaxProjectionSnapshots || len(pins) > MaxProjectionSnapshots {
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
	contributions := map[string]VerifiedContribution{}
	groups := map[string][]VerifiedContribution{}
	managed := map[domain.ContentHash]bool{}
	for _, contribution := range verified {
		if contribution.Record.RepoID != repo {
			return plan, domain.ErrIntegrity
		}
		contributions[contribution.Record.ID] = contribution
		key := contextprojection.ContributionMergeKey(contribution.Record.DestinationBranchID, contribution.Record.Capture, contribution.Record.IntegrationSHA)
		groups[key] = append(groups[key], contribution)
		managed[contribution.Record.Capture] = true
	}
	eligible := map[string]VerifiedContribution{}
	ordinaryRoots := []domain.ContentHash{inclusion.SnapshotID}
	for _, root := range inclusion.Roots {
		if !managed[root] {
			ordinaryRoots = append(ordinaryRoots, root)
		}
	}
	for _, merge := range inclusion.Merges {
		key := contextprojection.ContributionMergeKey(merge.DestinationBranchID, merge.Source, merge.MergeSHA)
		contribution, exists := contributions[merge.EventID]
		if len(groups[key]) == 0 && merge.State == "included" {
			ordinaryRoots = append(ordinaryRoots, merge.Source)
		}
		if exists && (merge.Source != contribution.Record.Capture || merge.DestinationBranchID != contribution.Record.DestinationBranchID || merge.MergeSHA != contribution.Record.IntegrationSHA) {
			return plan, domain.ErrIntegrity
		}
		if merge.State == "included" {
			if !allowed[merge.Source] {
				return plan, domain.ErrIntegrity
			}
			for _, candidate := range groups[key] {
				if candidate.Valid {
					eligible[candidate.Record.ID] = candidate
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
		if len(ordinary) > MaxProjectionSnapshots || edges > MaxProjectionEdges {
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
	versions := map[versionKey]*ContributionVersion{}
	seen := map[string]bool{}
	acceptedPins := map[ContributionPinKey]bool{}
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
		publication, exists := contribution.Publications[pin.PublicationID]
		if !exists {
			continue
		}
		memorySource := publication.MemorySource
		if memorySource == "" && publication.MemoryHash != "" {
			memorySource = contribution.Record.Capture
		}
		if publication.Kind != "publish" || publication.RepoID != string(repo) || publication.BranchID != contribution.Record.SourceBranchID || publication.Target != contribution.Record.Capture || publication.Source != publication.Target || publication.GitAfter != contribution.Record.OriginalHead || publication.MemoryHash != pin.MemoryHash || memorySource != pin.MemorySource || publication.MemoryPinned != pin.MemoryPinned {
			return plan, domain.ErrIntegrity
		}
		seen[pin.ID] = true
		acceptedPins[ContributionPinKey{pin.ContributionID, pin.PublicationID}] = true
		source := pin.MemorySource
		if source == "" {
			source = contribution.Record.Capture
		}
		key := versionKey{source, pin.MemoryHash}
		version := versions[key]
		if version == nil {
			version = &ContributionVersion{Source: source, Hash: pin.MemoryHash, State: ContributionSelected}
			versions[key] = version
		}
		version.Pins = append(version.Pins, pin)
	}
	publicationCount := 0
	for _, contribution := range eligible {
		for _, publication := range contribution.Publications {
			publicationCount++
			if publicationCount > MaxProjectionSnapshots {
				return plan, domain.ErrMemoryProjectionLimit
			}
			pinKey := ContributionPinKey{contribution.Record.ID, publication.ID}
			if acceptedPins[pinKey] {
				continue
			}
			source := publication.MemorySource
			if source == "" {
				source = contribution.Record.Capture
			}
			key := versionKey{source, publication.MemoryHash}
			version := versions[key]
			if version == nil {
				version = &ContributionVersion{Source: source, Hash: publication.MemoryHash}
				versions[key] = version
			}
			version.State = ContributionUnavailable
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
	reader := NewProjectionReader(source, repo)
	chains := map[domain.ContentHash]contributionMemoryParent{}
	steps := 0
	if err := validateContributionMemoryTopology(ctx, byID, plan.Versions); err != nil {
		return plan, err
	}
	for start := 0; start < len(plan.Versions); {
		end := start + 1
		for end < len(plan.Versions) && plan.Versions[end].Source == plan.Versions[start].Source {
			end++
		}
		if err := selectContributionMemoryVersions(ctx, reader, byID, plan.Versions[start:end], chains, &steps); err != nil {
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
				rank := plan.order[eligible[pin.ContributionID].Record.Capture]
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
	if len(metadata) > MaxContributionMetadataBytes {
		return plan, domain.ErrMemoryProjectionLimit
	}
	return plan, nil
}

type contributionMemoryParent struct{ source, parent domain.ContentHash }

func validateContributionMemoryTopology(ctx context.Context, snapshots map[domain.ContentHash]domain.Snapshot, versions []ContributionVersion) error {
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
		if len(states) >= MaxProjectionSnapshots {
			return domain.ErrMemoryProjectionLimit
		}
		snapshot, exists := snapshots[id]
		if !exists {
			return domain.ErrIntegrity
		}
		edges += len(snapshot.Parents)
		if edges > MaxProjectionEdges {
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
		if version.Hash != "" && version.State != ContributionUnavailable {
			if err := visit(version.Source); err != nil {
				return err
			}
		}
	}
	return nil
}

func selectContributionMemoryVersions(ctx context.Context, reader *ProjectionReader, snapshots map[domain.ContentHash]domain.Snapshot, versions []ContributionVersion, chains map[domain.ContentHash]contributionMemoryParent, steps *int) error {
	candidates := map[domain.ContentHash]int{}
	uncertain := false
	for index := range versions {
		if versions[index].State == ContributionUnavailable {
			uncertain = true
			continue
		}
		if versions[index].Hash == "" {
			versions[index].State = ContributionUnavailable
			for _, pin := range versions[index].Pins {
				if pin.MemoryPinned {
					versions[index].State = ContributionEmpty
				}
			}
			continue
		}
		candidates[versions[index].Hash] = index
	}
	superseded := map[domain.ContentHash]bool{}
	missing := map[domain.ContentHash]bool{}
	for _, version := range versions {
		if version.State == ContributionUnavailable {
			continue
		}
		visited := map[domain.ContentHash]bool{}
		for hash := version.Hash; hash != ""; {
			if err := ctx.Err(); err != nil {
				return err
			}
			*steps++
			if *steps > MaxProjectionEdges {
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
				if len(chains) >= MaxProjectionSnapshots {
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
				if reader.BytesRead() > MaxProjectionMemoryBytes {
					return domain.ErrMemoryProjectionLimit
				}
				if digest.SnapshotID != version.Source || domain.ValidateOptionalContentHash(digest.PreviousMemoryHash) != nil || digest.ValidateMemoryClaims() != nil {
					return domain.ErrIntegrity
				}
				parent = contributionMemoryParent{version.Source, digest.PreviousMemoryHash}
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
		if version.State == ContributionUnavailable {
			continue
		}
		if missing[version.Hash] {
			version.State = ContributionUnavailable
		} else if superseded[version.Hash] {
			version.State = ContributionSuperseded
		}
		if version.State == ContributionSelected || version.State == ContributionEmpty {
			tips++
		}
	}
	for index := range versions {
		version := &versions[index]
		if version.State == ContributionSuperseded || version.State == ContributionUnavailable {
			continue
		}
		if tips > 1 || uncertain {
			version.State = ContributionReview
		}
		if version.State != ContributionSelected {
			continue
		}
		pinned := contributionPinnedMemoryStore{reader: reader, snapshots: snapshots, source: version.Source, hash: version.Hash, steps: steps}
		digest, _, complete, err := ProjectLineage(ctx, pinned, version.Source)
		if err != nil {
			return err
		}
		if !complete {
			return domain.ErrIntegrity
		}
		if reader.BytesRead() > MaxProjectionMemoryBytes {
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

type contributionPinnedMemoryStore struct {
	reader       *ProjectionReader
	snapshots    map[domain.ContentHash]domain.Snapshot
	source, hash domain.ContentHash
	steps        *int
}

func (store contributionPinnedMemoryStore) GetSnapshot(ctx context.Context, id domain.ContentHash) (domain.Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return domain.Snapshot{}, err
	}
	*store.steps++
	if *store.steps > MaxProjectionEdges {
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

func (store contributionPinnedMemoryStore) GetMemory(ctx context.Context, hash domain.ContentHash) (domain.MemoryDigest, error) {
	return store.reader.GetMemory(ctx, hash)
}

func (plan ContributionPlan) Merge(ctx context.Context, projection domain.MemoryProjection) (domain.MemoryProjection, error) {
	if err := ctx.Err(); err != nil {
		return domain.MemoryProjection{}, err
	}
	if len(plan.excluded) == 0 && len(plan.Versions) == 0 {
		return projection, nil
	}
	for _, version := range plan.Versions {
		if !version.Independent && (version.State == ContributionReview || version.State == ContributionUnavailable) {
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
	accumulator := domain.NewMemoryAccumulator(MaxProjectionMemoryBytes)
	for _, version := range plan.Versions {
		if err := ctx.Err(); err != nil {
			return domain.MemoryProjection{}, err
		}
		if version.State != ContributionSelected || version.Independent {
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
			return MaxProjectionSnapshots
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
		Versions []ContributionVersion
	}{1, projection.StateHash, plan.Versions})
	if err != nil {
		return domain.MemoryProjection{}, err
	}
	if len(raw) > MaxContributionMetadataBytes {
		return domain.MemoryProjection{}, domain.ErrMemoryProjectionLimit
	}
	projection.StateHash = domain.HashContent(raw)
	return projection, nil
}
