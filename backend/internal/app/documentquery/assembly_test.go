package documentquery

import (
	"context"
	"errors"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

// Stand-ins that are only handed over: none of their methods is called by
// the assembly, and a nil pointer of each is the typed nil a caller could
// pass by mistake.
type assemblyScope struct {
	Scope
	readOnly []bool
}

func (scope *assemblyScope) AuthorizeDocumentRead(ctx context.Context, _ domain.ContentHash) (context.Context, error) {
	scope.readOnly = append(scope.readOnly, outbound.DocReadOnly(ctx))
	return ctx, nil
}

type assemblyLegacy struct{ LegacyReader }
type assemblyIndex struct{ outbound.DocReadStore }
type assemblyResources struct{ Resources }

// The reader is put together with a named profile, the scope that authorizes
// a read and the store it reads documents from. What is optional may be left
// out, and may not be a nil pointer dressed as a value.
func TestNewRequiresAProfileAndWhatItReadsWith(t *testing.T) {
	complete := func() Config {
		return Config{Profile: LegacyCompatibility, Scope: &assemblyScope{}, Legacy: &assemblyLegacy{}}
	}
	queries, err := New(complete())
	if err != nil || queries == nil || queries.index != nil || queries.resources != nil || queries.atomicRead || queries.readOnly {
		t.Fatalf("the least a reader needs: %+v %v", queries, err)
	}
	full := complete()
	full.Profile, full.Index, full.Resources, full.AtomicRead, full.ReadOnly = VerifiedRoots, &assemblyIndex{}, &assemblyResources{}, true, true
	queries, err = New(full)
	if err != nil || queries.scope != full.Scope || queries.legacy != full.Legacy || queries.index != full.Index || queries.resources != full.Resources || !queries.atomicRead || !queries.readOnly {
		t.Fatalf("everything a reader can be given: %+v %v", queries, err)
	}
	for name, test := range map[string]struct {
		change func(*Config)
		want   error
	}{
		"no profile":                 {func(config *Config) { config.Profile = 0 }, domain.ErrValidation},
		"an unknown profile":         {func(config *Config) { config.Profile = 3 }, domain.ErrValidation},
		"no scope":                   {func(config *Config) { config.Scope = nil }, domain.ErrValidation},
		"a nil pointer as scope":     {func(config *Config) { config.Scope = (*assemblyScope)(nil) }, domain.ErrValidation},
		"no document store":          {func(config *Config) { config.Legacy = nil }, domain.ErrValidation},
		"a nil pointer as store":     {func(config *Config) { config.Legacy = (*assemblyLegacy)(nil) }, domain.ErrValidation},
		"a nil pointer as index":     {func(config *Config) { config.Index = (*assemblyIndex)(nil) }, domain.ErrValidation},
		"a nil pointer as resources": {func(config *Config) { config.Resources = (*assemblyResources)(nil) }, domain.ErrValidation},
		// Documents of the root identity are read through the stored index only.
		"verified roots without the stored index": {func(config *Config) { config.Profile = VerifiedRoots }, domain.ErrUnsupportedDocumentIdentity},
	} {
		config := complete()
		test.change(&config)
		if queries, err := New(config); queries != nil || !errors.Is(err, test.want) {
			t.Errorf("%s: %v %v, want %v", name, queries, err, test.want)
		}
	}
}

// What counts as left out: nothing at all, or a nil of a kind that can be nil.
func TestAbsentTellsANilOfAnyKindFromAValue(t *testing.T) {
	var pointer *assemblyScope
	var mapping map[string]int
	var list []int
	var function func()
	var channel chan int
	for name, value := range map[string]any{"nothing": nil, "a nil pointer": pointer, "a nil map": mapping, "a nil slice": list, "a nil function": function, "a nil channel": channel} {
		if !absent(value) {
			t.Errorf("%s was taken for a value", name)
		}
	}
	for name, value := range map[string]any{"a pointer": &assemblyScope{}, "a struct": assemblyLegacy{}, "a number": 0, "an empty text": "",
		"an empty map": map[string]int{}, "an empty slice": []int{}, "a function": func() {}} {
		if absent(value) {
			t.Errorf("%s was taken for nothing", name)
		}
	}
}

// A reader made for a process that must not write tells so to everything it
// calls, through the context; another reader passes the context as it is.
func TestReadOnlyReaderSaysSoToWhatItCalls(t *testing.T) {
	for _, readOnly := range []bool{false, true} {
		scope := &assemblyScope{}
		queries, err := New(Config{Profile: LegacyCompatibility, Scope: scope, Legacy: &assemblyLegacy{}, ReadOnly: readOnly})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := queries.reauthorizeDocumentRead(context.Background(), domain.HashContent([]byte("repo"))); err != nil {
			t.Fatal(err)
		}
		if len(scope.readOnly) != 1 || scope.readOnly[0] != readOnly {
			t.Errorf("a reader made read-only=%v called its scope with %v", readOnly, scope.readOnly)
		}
	}
}
