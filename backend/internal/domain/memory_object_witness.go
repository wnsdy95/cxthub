package domain

import (
	"slices"
	"strings"
)

type MemoryObjectVersion struct {
	Hash        ContentHash `json:"hash"`
	Incarnation string      `json:"incarnation"`
	Version     int64       `json:"version,string"`
}

func (version MemoryObjectVersion) Validate() error {
	if ValidateContentHash(version.Hash) != nil || !catalogEpochValid(version.Incarnation) ||
		version.Incarnation == "00000000-0000-0000-0000-000000000000" || version.Version < 0 {
		return ErrIntegrity
	}
	return nil
}

type MemoryObjectWitness struct {
	RepoID     ContentHash           `json:"repo_id"`
	Hash       ContentHash           `json:"hash"`
	Available  bool                  `json:"available"`
	Root       *MemoryObjectVersion  `json:"root,omitempty"`
	Components []MemoryObjectVersion `json:"components,omitempty"`
}

func (witness MemoryObjectWitness) Validate() error {
	if ValidateContentHash(witness.RepoID) != nil || ValidateContentHash(witness.Hash) != nil {
		return ErrIntegrity
	}
	if len(witness.Components) >= MaxMemoryCaptureObjects {
		return ErrMemoryProjectionLimit
	}
	if !witness.Available {
		if witness.Root != nil || len(witness.Components) != 0 {
			return ErrIntegrity
		}
		return nil
	}
	if witness.Root == nil || witness.Root.Hash != witness.Hash || witness.Root.Validate() != nil {
		return ErrIntegrity
	}
	for ordinal, component := range witness.Components {
		if component.Validate() != nil || ordinal > 0 && witness.Components[ordinal-1].Hash >= component.Hash {
			return ErrIntegrity
		}
	}
	return nil
}

func (witness MemoryObjectWitness) Clone() MemoryObjectWitness {
	if witness.Root != nil {
		root := *witness.Root
		witness.Root = &root
	}
	witness.Components = slices.Clone(witness.Components)
	return witness
}

type CapturedMemoryObject struct {
	Digest  *MemoryDigest
	Witness MemoryObjectWitness
}

type MemoryObjectDependency struct {
	Kind    string
	Hash    ContentHash
	Version *MemoryObjectVersion
}

type MemoryObjectWitnessSet struct {
	repo    ContentHash
	roots   map[ContentHash]bool
	objects map[[2]string]*MemoryObjectVersion
	failure error
}

func NewMemoryObjectWitnessSet(repo ContentHash) *MemoryObjectWitnessSet {
	set := &MemoryObjectWitnessSet{repo: repo, roots: make(map[ContentHash]bool), objects: make(map[[2]string]*MemoryObjectVersion)}
	if ValidateContentHash(repo) != nil {
		set.failure = ErrIntegrity
	}
	return set
}

func (set *MemoryObjectWitnessSet) Add(witness MemoryObjectWitness) error {
	if set.failure != nil {
		return set.failure
	}
	set.failure = set.add(witness)
	return set.failure
}

func (set *MemoryObjectWitnessSet) add(witness MemoryObjectWitness) error {
	if err := witness.Validate(); err != nil {
		return err
	}
	if witness.RepoID != set.repo || set.roots[witness.Hash] {
		return ErrIntegrity
	}
	set.roots[witness.Hash] = true
	add := func(kind string, hash ContentHash, version *MemoryObjectVersion) error {
		identity := [2]string{kind, string(hash)}
		if previous, exists := set.objects[identity]; exists {
			if previous == nil || version == nil {
				if previous != version {
					return ErrIntegrity
				}
			} else if *previous != *version {
				return ErrIntegrity
			}
			return nil
		}
		if len(set.objects) == MaxMemoryCaptureObjects {
			return ErrMemoryProjectionLimit
		}
		if version != nil {
			owned := *version
			version = &owned
		}
		set.objects[identity] = version
		return nil
	}
	if err := add("memory", witness.Hash, witness.Root); err != nil {
		return err
	}
	for _, component := range witness.Components {
		if err := add("memory_chunk", component.Hash, &component); err != nil {
			return err
		}
	}
	return nil
}

func (set *MemoryObjectWitnessSet) Objects() ([]MemoryObjectDependency, error) {
	if set.failure != nil {
		return nil, set.failure
	}
	objects := make([]MemoryObjectDependency, 0, len(set.objects))
	for identity, version := range set.objects {
		if version != nil {
			owned := *version
			version = &owned
		}
		objects = append(objects, MemoryObjectDependency{Kind: identity[0], Hash: ContentHash(identity[1]), Version: version})
	}
	slices.SortFunc(objects, func(left, right MemoryObjectDependency) int {
		if compared := strings.Compare(left.Kind, right.Kind); compared != 0 {
			return compared
		}
		return strings.Compare(string(left.Hash), string(right.Hash))
	})
	return objects, nil
}
