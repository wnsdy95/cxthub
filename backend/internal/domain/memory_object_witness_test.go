package domain

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestMemoryObjectWitnessValidationAndOwnership(test *testing.T) {
	root := MemoryObjectVersion{Hash: HashContent([]byte("root")), Incarnation: "11111111-2222-3333-4444-555555555555", Version: 1<<63 - 1}
	component := root
	component.Hash = HashContent([]byte("component"))
	witness := MemoryObjectWitness{RepoID: HashContent([]byte("repository")), Hash: root.Hash, Available: true, Root: &root, Components: []MemoryObjectVersion{component}}
	if err := witness.Validate(); err != nil {
		test.Fatal(err)
	}
	raw, err := json.Marshal(witness)
	if err != nil || !strings.Contains(string(raw), `"version":"9223372036854775807"`) {
		test.Fatal("version lost wire precision", string(raw), err)
	}
	var restored MemoryObjectWitness
	if err := json.Unmarshal(raw, &restored); err != nil || !reflect.DeepEqual(restored, witness) {
		test.Fatal("witness round trip", err)
	}
	clone := witness.Clone()
	clone.Root.Version = 0
	clone.Components[0].Hash = "invalid"
	if !reflect.DeepEqual(restored, witness) {
		test.Fatal("clone aliases caller state")
	}
	for _, scenario := range []string{"repo", "hash", "root", "foreign-root", "incarnation", "zero-incarnation", "version", "duplicate", "absent-root", "absent-components", "too-many"} {
		test.Run(scenario, func(test *testing.T) {
			invalid := witness.Clone()
			wanted := ErrIntegrity
			switch scenario {
			case "repo":
				invalid.RepoID = ""
			case "hash":
				invalid.Hash = "invalid"
			case "root":
				invalid.Root = nil
			case "foreign-root":
				invalid.Root.Hash = component.Hash
			case "incarnation":
				invalid.Root.Incarnation = "invalid"
			case "zero-incarnation":
				invalid.Root.Incarnation = "00000000-0000-0000-0000-000000000000"
			case "version":
				invalid.Root.Version = -1
			case "duplicate":
				invalid.Components = append(invalid.Components, component)
			case "absent-root":
				invalid.Available = false
			case "absent-components":
				invalid.Available, invalid.Root = false, nil
			case "too-many":
				invalid.Components = make([]MemoryObjectVersion, MaxMemoryCaptureObjects+1)
				wanted = ErrMemoryProjectionLimit
			}
			if !errors.Is(invalid.Validate(), wanted) {
				test.Fatal("invalid witness accepted", invalid.Validate())
			}
		})
	}
	absent := MemoryObjectWitness{RepoID: witness.RepoID, Hash: witness.Hash}
	if err := absent.Validate(); err != nil || !reflect.DeepEqual(absent.Clone(), absent) {
		test.Fatal("explicit absence rejected", err)
	}
}

func TestMemoryObjectWitnessSetBoundariesAndIsolation(test *testing.T) {
	repo := HashContent([]byte(test.Name()))
	makeWitness := func(label string) MemoryObjectWitness {
		hash := HashContent([]byte(label))
		return MemoryObjectWitness{RepoID: repo, Hash: hash, Available: true, Root: &MemoryObjectVersion{Hash: hash, Incarnation: "11111111-2222-3333-4444-555555555555"}}
	}
	left, right := makeWitness("left"), makeWitness("right")
	component := *makeWitness("component").Root
	left.Components = []MemoryObjectVersion{component}
	right.Components = []MemoryObjectVersion{component}
	set := NewMemoryObjectWitnessSet(repo)
	if err := set.Add(left); err != nil {
		test.Fatal(err)
	}
	left.Root.Version++
	left.Components[0].Version++
	if err := set.Add(right); err != nil {
		test.Fatal("caller mutation changed retained dependency versions", err)
	}
	objects, err := set.Objects()
	if err != nil || len(objects) != 3 {
		test.Fatal("shared component was counted twice", err)
	}
	objects[0].Version.Version++
	fresh, err := set.Objects()
	if err != nil || fresh[0].Version.Version != 0 {
		test.Fatal("returned dependency aliases private set", err)
	}
	conflict := makeWitness("conflict")
	conflict.Components = []MemoryObjectVersion{component}
	conflict.Components[0].Version++
	if err := set.Add(conflict); !errors.Is(err, ErrIntegrity) {
		test.Fatal("mixed component versions accepted", err)
	}
	if objects, err := set.Objects(); !errors.Is(err, ErrIntegrity) || objects != nil {
		test.Fatal("failed set returned partial closure", err)
	}
	if err := set.Add(makeWitness("after failure")); !errors.Is(err, ErrIntegrity) {
		test.Fatal("failed set resumed", err)
	}
	largest := makeWitness("maximum")
	for ordinal := range MaxMemoryCaptureObjects - 1 {
		largest.Components = append(largest.Components, *makeWitness(fmt.Sprint("chunk", ordinal)).Root)
	}
	slices.SortFunc(largest.Components, func(left, right MemoryObjectVersion) int {
		return strings.Compare(string(left.Hash), string(right.Hash))
	})
	set = NewMemoryObjectWitnessSet(repo)
	if err := set.Add(largest); err != nil {
		test.Fatal("exact root-plus-components limit rejected", err)
	}
	if objects, err := set.Objects(); err != nil || len(objects) != MaxMemoryCaptureObjects {
		test.Fatal("complete maximum dependency set rejected", err)
	}
	if err := set.Add(makeWitness("one beyond limit")); !errors.Is(err, ErrMemoryProjectionLimit) {
		test.Fatal("multiple-root closure bypassed total object limit", err)
	}
	if objects, err := set.Objects(); !errors.Is(err, ErrMemoryProjectionLimit) || objects != nil {
		test.Fatal("oversized closure exposed partial objects", err)
	}
	largest.Components = append(largest.Components, component)
	if err := NewMemoryObjectWitnessSet(repo).Add(largest); !errors.Is(err, ErrMemoryProjectionLimit) {
		test.Fatal("single root failed to reserve its own object slot", err)
	}
}
