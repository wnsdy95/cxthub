package app

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub-cli/internal/domain"
)

type branchIDStoreFake struct {
	branches []domain.LocalBranch
	stored   []domain.BranchIDAssignment
	reads    int
	locks    int
	locked   bool
	readErr  error
	storeErr error
}

func (f *branchIDStoreFake) WithBranchIDLock(_ context.Context, _ string, use func() error) error {
	f.locks++
	if f.locked {
		return errors.New("the lock was taken twice")
	}
	f.locked = true
	defer func() { f.locked = false }()
	return use()
}

func (f *branchIDStoreFake) LocalBranches(context.Context, string) ([]domain.LocalBranch, error) {
	if !f.locked {
		return nil, errors.New("branches were read outside the lock")
	}
	f.reads++
	return append([]domain.LocalBranch(nil), f.branches...), f.readErr
}

func (f *branchIDStoreFake) StoreBranchID(_ context.Context, _ string, assignment domain.BranchIDAssignment) error {
	if f.storeErr != nil {
		return f.storeErr
	}
	f.stored = append(f.stored, assignment)
	for index := range f.branches {
		if f.branches[index].Name == assignment.Name {
			f.branches[index].ID, f.branches[index].IssuedFor = assignment.ID, assignment.Name
		}
	}
	return nil
}

func TestEnsureBranchIDsStoresOnlyWhatIsMissing(t *testing.T) {
	a, b := strings.Repeat("a", 32), strings.Repeat("b", 32)
	newID := func() (string, error) { return b, nil }
	store := &branchIDStoreFake{branches: []domain.LocalBranch{{Name: "main", ID: a, IssuedFor: "main"}, {Name: "topic"}}}
	got, assigned, err := EnsureBranchIDs(context.Background(), store, "/work/app", newID)
	want := []domain.LocalBranch{{Name: "main", ID: a, IssuedFor: "main"}, {Name: "topic", ID: b, IssuedFor: "topic"}}
	if err != nil || assigned != 1 || !reflect.DeepEqual(got, want) || !reflect.DeepEqual(store.stored, []domain.BranchIDAssignment{{Name: "topic", ID: b}}) || store.reads != 2 || store.locks != 1 {
		t.Fatalf("got=%+v assigned=%d stored=%+v reads=%d locks=%d err=%v", got, assigned, store.stored, store.reads, store.locks, err)
	}
	// Nothing missing: nothing written, and one read.
	store.stored, store.reads = nil, 0
	if got, assigned, err := EnsureBranchIDs(context.Background(), store, "/work/app", newID); err != nil || assigned != 0 || !reflect.DeepEqual(got, want) || store.stored != nil || store.reads != 1 {
		t.Fatalf("second run: got=%+v stored=%+v reads=%d err=%v", got, store.stored, store.reads, err)
	}
}

func TestEnsureBranchIDsStopsOnFailure(t *testing.T) {
	failure := errors.New("synthetic failure")
	newID := func() (string, error) { return strings.Repeat("b", 32), nil }
	if _, _, err := EnsureBranchIDs(context.Background(), &branchIDStoreFake{readErr: failure}, "/work/app", newID); !errors.Is(err, failure) {
		t.Fatalf("read failure: %v", err)
	}
	store := &branchIDStoreFake{branches: []domain.LocalBranch{{Name: "topic"}}, storeErr: failure}
	if _, _, err := EnsureBranchIDs(context.Background(), store, "/work/app", newID); !errors.Is(err, failure) {
		t.Fatalf("store failure: %v", err)
	}
	// No randomness, no ID: nothing is written.
	store = &branchIDStoreFake{branches: []domain.LocalBranch{{Name: "topic"}}}
	if _, _, err := EnsureBranchIDs(context.Background(), store, "/work/app", func() (string, error) { return "", failure }); !errors.Is(err, failure) || store.stored != nil {
		t.Fatalf("ID failure: %v stored=%+v", err, store.stored)
	}
	if _, _, err := EnsureBranchIDs(context.Background(), nil, "/work/app", newID); err == nil {
		t.Fatal("an unwired store must fail")
	}
}
