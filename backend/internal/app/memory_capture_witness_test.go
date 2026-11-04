package app

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func witnessedMemoryFixture(test *testing.T) *CapturedEffectiveMemory {
	test.Helper()
	captured, _ := capturedMemoryFixture(test)
	hash := captured.Dependencies()[0].Hash
	captured.witnesses = map[domain.ContentHash]domain.MemoryObjectWitness{
		hash: {
			RepoID: captured.reader.repo, Hash: hash, Available: true,
			Root:       &domain.MemoryObjectVersion{Hash: hash, Incarnation: "12345678-abcd-4321-9876-123456789abc", Version: 7},
			Components: []domain.MemoryObjectVersion{{Hash: hh("component"), Incarnation: "23456789-abcd-4321-9876-123456789abc", Version: 3}},
		},
	}
	return captured
}

func TestCapturedMemoryWitnessOwnershipAndClosure(test *testing.T) {
	ctx := context.Background()
	captured := witnessedMemoryFixture(test)
	absent := hh("absent")
	captured.reader.objects[absent] = nil
	captured.witnesses[absent] = domain.MemoryObjectWitness{RepoID: captured.reader.repo, Hash: absent}
	original, err := captured.ObjectWitnesses(ctx)
	if err != nil || len(original) != 2 || original[0].Hash >= original[1].Hash {
		test.Fatal("witnesses are not a sorted complete dependency set", err)
	}
	for ordinal := range original {
		if original[ordinal].Available {
			original[ordinal].Root.Version++
			original[ordinal].Components[0].Version++
		}
	}
	fresh, err := captured.ObjectWitnesses(ctx)
	if err != nil || reflect.DeepEqual(fresh, original) {
		test.Fatal("caller changed private witness state", err)
	}
	for _, witness := range fresh {
		if !reflect.DeepEqual(witness, captured.witnesses[witness.Hash]) {
			test.Fatal("private dependency witness changed")
		}
	}
	for _, mode := range []string{"unsupported", "missing", "extra", "foreign", "wrong hash", "wrong availability", "invalid root", "nil capture", "canceled"} {
		test.Run(mode, func(test *testing.T) {
			captured := witnessedMemoryFixture(test)
			hash := captured.Dependencies()[0].Hash
			witness := captured.witnesses[hash]
			bound, cancel := context.WithCancel(ctx)
			defer cancel()
			wanted := domain.ErrIntegrity
			switch mode {
			case "unsupported":
				captured.witnesses = nil
				wanted = domain.ErrCatalogUnsupported
			case "missing":
				delete(captured.witnesses, hash)
			case "extra":
				captured.witnesses[absent] = domain.MemoryObjectWitness{RepoID: captured.reader.repo, Hash: absent}
			case "foreign":
				witness.RepoID = hh("foreign")
				captured.witnesses[hash] = witness
			case "wrong hash":
				witness.Hash = absent
				captured.witnesses[hash] = witness
			case "wrong availability":
				captured.reader.objects[hash] = nil
			case "invalid root":
				witness.Root.Incarnation = ""
			case "nil capture":
				captured = nil
			case "canceled":
				cancel()
				wanted = context.Canceled
			}
			if result, err := captured.ObjectWitnesses(bound); !errors.Is(err, wanted) || result != nil {
				test.Fatalf("returned incomplete witnesses: %v, want %v", err, wanted)
			}
		})
	}
}
