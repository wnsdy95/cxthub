package domain

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

func catalogInvalid(message string) error {
	return fmt.Errorf("%w: catalog %s", ErrHashMismatch, message)
}

// ValidateCatalogCheckpoint checks the repository-scoped v1 acquisition cursor.
// Epoch is an opaque UUID; it is compared exactly, never inferred from a ref.
func ValidateCatalogCheckpoint(repo string, cp CatalogCheckpoint) error {
	if ValidateContentHash(ContentHash(repo)) != nil || cp.Version != CatalogVersion || cp.RepoID != repo || !catalogEpochValid(cp.Epoch) || cp.Sequence < 0 {
		return catalogInvalid("checkpoint identity or sequence")
	}
	return nil
}

func catalogEpochValid(epoch string) bool {
	if len(epoch) != 36 {
		return false
	}
	for i := range len(epoch) {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if epoch[i] != '-' {
				return false
			}
			continue
		}
		c := epoch[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

func (r CatalogRequest) Validate() error {
	if r.Version != CatalogVersion || r.Limit < 0 || r.Limit > MaxCatalogLimit || (r.After != nil && r.Cursor != "") {
		return catalogInvalid("request shape")
	}
	if r.After != nil {
		if r.After.Version != CatalogVersion {
			return ErrCatalogResetRequired
		}
		return ValidateCatalogCheckpoint(r.After.RepoID, *r.After)
	}
	return nil
}

// ValidateCatalogEntry validates one after-image, never its graph closure. A
// transaction can publish referenced objects on a later page of the same run.
func ValidateCatalogEntry(repo string, entry CatalogEntry) error {
	_, err := decodeCatalogEntry(repo, entry)
	return err
}

type catalogEntity struct {
	snapshot Snapshot
	ref      Ref
	protocol int
}

type catalogProtocol struct {
	ContextProtocol int `json:"context_protocol"`
}

func catalogRefKey(key string) ([2]string, error) {
	var names []string
	if err := decodeCatalogJSON([]byte(key), &names); err != nil || len(names) != 2 {
		return [2]string{}, catalogInvalid("ref key")
	}
	if ValidateRefName(names[0], names[1]) != nil {
		return [2]string{}, catalogInvalid("ref key identity")
	}
	return [2]string{names[0], names[1]}, nil
}

func catalogHistoryIDValid(id string) bool {
	if len(id) != 32 || id != strings.ToLower(id) {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil
}

func decodeCatalogEntry(repo string, e CatalogEntry) (catalogEntity, error) {
	var out catalogEntity
	if ValidateContentHash(ContentHash(repo)) != nil || e.Sequence < 0 {
		return out, catalogInvalid("entry scope or sequence")
	}
	var refKey [2]string
	switch e.Kind {
	case "snapshot":
		if ValidateContentHash(ContentHash(e.Key)) != nil {
			return out, catalogInvalid("snapshot key")
		}
	case "ref":
		var err error
		refKey, err = catalogRefKey(e.Key)
		if err != nil {
			return out, err
		}
	case "history":
		if !catalogHistoryIDValid(e.Key) {
			return out, catalogInvalid("history key")
		}
	case "protocol":
		if e.Key != repo {
			return out, catalogInvalid("protocol key")
		}
	default:
		return out, catalogInvalid("unknown entry kind")
	}
	if e.Deleted {
		if len(e.Value) != 0 {
			return out, catalogInvalid("tombstone has a value")
		}
		return out, nil
	}
	switch e.Kind {
	case "snapshot":
		if err := decodeCatalogJSON(e.Value, &out.snapshot); err != nil {
			return out, err
		}
		s := out.snapshot
		if s.RepoID != repo || string(s.ID) != e.Key || validateCatalogSnapshot(s) != nil {
			return out, catalogInvalid("snapshot metadata")
		}
	case "ref":
		if err := decodeCatalogJSON(e.Value, &out.ref); err != nil {
			return out, err
		}
		r := out.ref
		if r.RepoID != repo || r.Kind != refKey[0] || r.Name != refKey[1] || ValidateRef(r) != nil || (r.Kind != RefHEAD && r.Symbolic != "") {
			return out, catalogInvalid("ref metadata")
		}
		if _, _, err := ParseBranchLifecycleRef(r); err != nil {
			return out, catalogInvalid("lifecycle ref metadata")
		}
	case "history":
		var event HistoryEvent
		if err := decodeCatalogJSON(e.Value, &event); err != nil {
			return out, err
		}
		if event.RepoID != repo || event.ID != e.Key || ValidateHistoryEvent(event) != nil {
			return out, catalogInvalid("history metadata")
		}
	case "protocol":
		var protocol catalogProtocol
		if err := decodeCatalogJSON(e.Value, &protocol); err != nil {
			return out, err
		}
		if protocol.ContextProtocol < 0 || protocol.ContextProtocol > 1 {
			return out, catalogInvalid("unsupported context protocol")
		}
		out.protocol = protocol.ContextProtocol
	}
	return out, nil
}

// Match the object adapter's identity/hash validation without importing an
// adapter into domain. Do not invent newer required presentation metadata for
// historical objects accepted by the existing pull path.
func validateCatalogSnapshot(s Snapshot) error {
	if ValidateContentHash(s.ID) != nil || ValidateContentHash(s.DocHash) != nil || s.ID != s.DocHash || s.GraftSeq > MaxGraftSeq || s.CompactionCount < 0 {
		return catalogInvalid("snapshot identity or scalar")
	}
	for _, hash := range []ContentHash{s.MemoryHash, s.ClaudeSettings, s.AgentsSettings, s.CodexSettings} {
		if ValidateOptionalContentHash(hash) != nil {
			return catalogInvalid("snapshot attachment")
		}
	}
	for _, parents := range [][]ContentHash{s.Parents, s.GraftParents} {
		for _, parent := range parents {
			if ValidateContentHash(parent) != nil {
				return catalogInvalid("snapshot parent")
			}
		}
	}
	_, err := SnapshotStateHash(s)
	return err
}

// ValidateCatalogPage binds every page to the original acquisition request.
// previous is the already validated immediately preceding page, if any. Keep
// after unchanged for the entire delta run; an entry sequence is not an ack.
func ValidateCatalogPage(repo string, after *CatalogCheckpoint, previous *CatalogPage, page CatalogPage) error {
	if err := validateCatalogPageShape(repo, after, page); err != nil {
		return err
	}
	if previous == nil {
		return nil
	}
	if err := validateCatalogPageShape(repo, after, *previous); err != nil {
		return err
	}
	if previous.NextCursor == "" || previous.Checkpoint != nil || previous.Epoch != page.Epoch || previous.Through != page.Through || previous.Mode != page.Mode || (page.NextCursor != "" && previous.NextCursor == page.NextCursor) {
		return catalogInvalid("continuation boundary")
	}
	if len(page.Entries) != 0 {
		tail := previous.Entries[len(previous.Entries)-1]
		if !catalogEntryLess(tail, page.Entries[0], page.Mode) {
			return catalogInvalid("cross-page order")
		}
		// Equivalent JSON ref keys must not evade transaction/key uniqueness.
		oldIdentities := make(map[string]int64, len(previous.Entries))
		for _, old := range previous.Entries {
			oldIdentities[CatalogEntryIdentity(old)] = old.Sequence
		}
		for _, next := range page.Entries {
			if seq, exists := oldIdentities[CatalogEntryIdentity(next)]; exists && (page.Mode == "baseline" || seq == next.Sequence) {
				return catalogInvalid("cross-page duplicate identity")
			}
		}
	}
	return nil
}

func validateCatalogPageShape(repo string, after *CatalogCheckpoint, p CatalogPage) error {
	cp := CatalogCheckpoint{Version: p.Version, RepoID: p.RepoID, Epoch: p.Epoch, Sequence: p.Through}
	if err := ValidateCatalogCheckpoint(repo, cp); err != nil {
		return err
	}
	if p.Entries == nil || len(p.Entries) > MaxCatalogLimit {
		return catalogInvalid("entry count")
	}
	if after == nil {
		if p.Mode != "baseline" {
			return catalogInvalid("expected baseline")
		}
	} else if ValidateCatalogCheckpoint(repo, *after) != nil || p.Mode != "delta" || p.Epoch != after.Epoch || p.Through < after.Sequence {
		return catalogInvalid("delta checkpoint boundary")
	}
	if p.NextCursor != "" {
		if p.Checkpoint != nil || len(p.Entries) == 0 {
			return catalogInvalid("partial page progress")
		}
	} else if p.Checkpoint == nil || *p.Checkpoint != cp {
		return catalogInvalid("final page checkpoint")
	}
	seen := make(map[string]int64, len(p.Entries))
	for i, entry := range p.Entries {
		if err := ValidateCatalogEntry(repo, entry); err != nil {
			return err
		}
		if entry.Sequence > p.Through || (after != nil && entry.Sequence <= after.Sequence) || (p.Mode == "baseline" && entry.Deleted) {
			return catalogInvalid("entry outside page boundary")
		}
		if i > 0 && !catalogEntryLess(p.Entries[i-1], entry, p.Mode) {
			return catalogInvalid("entry order")
		}
		identity := CatalogEntryIdentity(entry)
		if sequence, exists := seen[identity]; exists && (p.Mode == "baseline" || sequence == entry.Sequence) {
			return catalogInvalid("duplicate entry identity")
		}
		seen[identity] = entry.Sequence
	}
	return nil
}

func catalogEntryLess(a, b CatalogEntry, mode string) bool {
	if mode == "delta" && a.Sequence != b.Sequence {
		return a.Sequence < b.Sequence
	}
	if a.Kind != b.Kind {
		return a.Kind < b.Kind
	}
	// Go string ordering matches PostgreSQL's C/UTF-8 byte ordering.
	return a.Key < b.Key
}

// CatalogEntryIdentity identifies a previously validated entry independently
// of JSON whitespace/escaping in a ref key. It is not the wire ordering key.
// Callers must first use ValidateCatalogEntry; invalid ref keys return "".
func CatalogEntryIdentity(e CatalogEntry) string {
	if e.Kind == "ref" {
		key, err := catalogRefKey(e.Key)
		if err != nil {
			return ""
		}
		raw, _ := json.Marshal(key)
		return e.Kind + "\x00" + string(raw)
	}
	return e.Kind + "\x00" + e.Key
}

// CatalogManifest projects a complete live image, not a delta journal. Raw
// entries remain untouched for history consumers and future acquisitions.
// Document/attachment verification and graph closure remain the pull path's
// responsibility. Catalog acquisition must never populate verified haves.
func CatalogManifest(repo string, entries []CatalogEntry) (Manifest, []Snapshot, error) {
	manifest := Manifest{RepoID: repo, Refs: []Ref{}, SnapshotIndex: []ContentHash{}, MemoryAttachments: map[ContentHash]ContentHash{}, SnapshotStates: map[ContentHash]ContentHash{}}
	snapshots := []Snapshot{}
	seen := make(map[string]bool, len(entries))
	protocols := 0
	for _, entry := range entries {
		entity, err := decodeCatalogEntry(repo, entry)
		if err != nil {
			return Manifest{}, nil, err
		}
		identity := CatalogEntryIdentity(entry)
		if entry.Deleted || seen[identity] {
			return Manifest{}, nil, catalogInvalid("complete image contains deletion or duplicate")
		}
		seen[identity] = true
		switch entry.Kind {
		case "snapshot":
			snapshots = append(snapshots, entity.snapshot)
		case "ref":
			manifest.Refs = append(manifest.Refs, entity.ref)
		case "protocol":
			protocols++
			manifest.ContextProtocol = entity.protocol
		}
	}
	if protocols != 1 {
		return Manifest{}, nil, catalogInvalid("complete image needs exactly one protocol")
	}
	if manifest.ContextProtocol == 0 {
		var err error
		manifest.Refs, err = ProjectBranchLifecycleRefs(manifest.Refs)
		if err != nil {
			return Manifest{}, nil, err
		}
	}
	sort.Slice(manifest.Refs, func(i, j int) bool {
		a, b := manifest.Refs[i], manifest.Refs[j]
		return a.Kind < b.Kind || (a.Kind == b.Kind && a.Name < b.Name)
	})
	sort.Slice(snapshots, func(i, j int) bool { return snapshots[i].ID < snapshots[j].ID })
	for _, snapshot := range snapshots {
		state, err := SnapshotStateHash(snapshot)
		if err != nil {
			return Manifest{}, nil, err
		}
		manifest.SnapshotIndex = append(manifest.SnapshotIndex, snapshot.ID)
		manifest.SnapshotStates[snapshot.ID] = state
		if snapshot.MemoryHash != "" {
			manifest.MemoryAttachments[snapshot.ID] = snapshot.MemoryHash
		}
	}
	manifest.Version = len(snapshots) // Match the existing PostgreSQL manifest.
	manifest.Version = len(snapshots)
	return manifest, snapshots, nil
}
