package domain

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func legacyNativeSharedW92Fixture(test *testing.T, distinct bool) (*legacyNativeObjectRestored, legacyNativeAnyW78Request, *legacyNativeAnyW78CAS) {
	test.Helper()
	raw := legacyNativeSharedW92Raw(false)
	if distinct {
		raw = strings.Replace(raw, `"output":{"kept":true`, `"output":{"kept":false`, 1)
	}
	request, native, scanner, source := legacyNativeEventW82FencePrepare(test, raw, nil, legacyNativeSharedW92Cut)
	storage := legacyNativeAnyW78Store(test)
	storage.preload(test, request)
	return &legacyNativeObjectRestored{Object: native, Scanner: *scanner, Source: source}, request, storage
}

func TestLegacyNativeObjectCheckpointW92LegacyFieldLocalNamespace(test *testing.T) {
	for _, distinct := range []bool{false, true} {
		name := "equal-values"
		if distinct {
			name = "different-values"
		}
		test.Run(name, func(test *testing.T) {
			bundle, request, storage := legacyNativeSharedW92Fixture(test, distinct)
			request = legacyNativeEventW82FenceCapture(test, bundle.Object, &bundle.Scanner, request, bundle.Source, storage)
			var state legacyNativeObjectCheckpointControl
			if err := legacyNativeCheckpointReadJSON(context.Background(), storage.io(), request.Root, &state); err != nil || state.AnyGraphNamespace != 1 {
				test.Fatal("actual cached parent lacks the explicit shared namespace", err)
			}
			state.AnyGraphNamespace = 0
			for _, name := range []string{"input", "output"} {
				index := bundle.Object.metadata.match(name)
				work, err := newLegacyNativeObjectAnyCheckpoint(context.Background(), bundle.Object, index, &bundle.Scanner, request.Binding, bundle.Source, storage.io(), legacyNativeEventW82FenceBudget)
				if err != nil {
					test.Fatal(err)
				}
				defer work.close()
				legacyNativeAnyFieldW84Ready(test, work, storage)
				saved, err := work.take(context.Background())
				if err != nil {
					test.Fatal(err)
				}
				state.Fields[index].Any = &saved
			}
			var err error
			request.Root, err = legacyNativeCheckpointWriteJSON(context.Background(), storage.io(), state)
			if err != nil {
				test.Fatal(err)
			}
			restored := legacyNativeEventW82FenceRestore(test, request, storage)
			value := restored.Object.destination.Interface().(*wire)
			input := legacyNativeAnyFieldW84Parser(restored.Object, "input")
			output := legacyNativeAnyFieldW84Parser(restored.Object, "output")
			if input.rootMap == output.rootMap || reflect.ValueOf(value.Input).Pointer() == reflect.ValueOf(value.Output).Pointer() ||
				!reflect.DeepEqual(value, bundle.Object.destination.Interface()) {
				test.Fatal("legacy field-local identities were globally combined or rejected")
			}
		})
	}
}

func TestLegacyNativeObjectCheckpointW92NamespaceValidation(test *testing.T) {
	bundle, request, storage := legacyNativeSharedW92Fixture(test, false)
	request = legacyNativeEventW82FenceCapture(test, bundle.Object, &bundle.Scanner, request, bundle.Source, storage)
	for _, name := range []string{"unknown-mode", "missing-cache", "mixed-initial-graph"} {
		test.Run(name, func(test *testing.T) {
			var state legacyNativeObjectCheckpointControl
			if err := legacyNativeCheckpointReadJSON(context.Background(), storage.io(), request.Root, &state); err != nil {
				test.Fatal(err)
			}
			switch name {
			case "unknown-mode":
				state.AnyGraphNamespace = 2
			case "missing-cache":
				state.HasContainerCache = false
			case "mixed-initial-graph":
				state.InitialGraphCount = 1
			}
			if err := state.validate(legacyNativeCheckpointEventModel, request.Binding, bundle.Source.Bytes, true); !errors.Is(err, ErrIntegrity) {
				test.Fatal("invalid graph identity namespace was admitted", err)
			}
		})
	}
}

func legacyNativeSharedW92CaptureGraph(test *testing.T, bundle *legacyNativeObjectRestored, request legacyNativeAnyW78Request, storage *legacyNativeAnyW78CAS) (*legacyNativeObjectCheckpoint, *legacyNativeObjectAnyCheckpoint, *legacyNativeCheckpointGraphWriter) {
	test.Helper()
	root, err := newLegacyNativeObjectCheckpoint(context.Background(), bundle.Object, &bundle.Scanner, request.Binding, bundle.Source, storage.io(), legacyNativeEventW82FenceBudget)
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(root.close)
	for attempt := 0; attempt < legacyNativeAnyFieldW84FenceSteps; attempt++ {
		field, ok := root.anyTask.(*legacyNativeObjectAnyCheckpoint)
		if ok && field.child != nil && field.child.graph != nil {
			graph, valid := field.child.graph.(*legacyNativeCheckpointGraphWriter)
			if !valid {
				test.Fatal("actual nested graph writer has an unexpected implementation")
			}
			test.Cleanup(field.close)
			return root, field, graph
		}
		if ready, err := root.step(context.Background()); err != nil || ready {
			test.Fatal("actual parent did not expose a live nested graph writer", ready, err)
		}
	}
	test.Fatal("nested graph setup exceeded the unchanged finite schedule")
	return nil, nil, nil
}

func TestLegacyNativeObjectCheckpointW92SharedCaptureFences(test *testing.T) {
	for _, name := range []string{"budget", "field-detached", "graph-detached", "source-generation", "parser-control", "parent-close", "ready-budget"} {
		test.Run(name, func(test *testing.T) {
			bundle, request, storage := legacyNativeSharedW92Fixture(test, false)
			root, field, graph := legacyNativeSharedW92CaptureGraph(test, bundle, request, storage)
			want := ErrConflict
			switch name {
			case "budget":
				root.budget, want = 1, ErrDocumentResourceLimit
			case "field-detached":
				root.anyTask = nil
			case "graph-detached":
				field.child.graph = nil
				test.Cleanup(graph.close)
			case "source-generation":
				field.sources.original.authority++
				want = ErrIntegrity
			case "parser-control":
				field.child.parser.position++
			case "parent-close":
				root.close()
				want = ErrIntegrity
			case "ready-budget":
				legacyNativeAnyFieldW84Ready(test, graph, storage)
				root.budget, want = 1, ErrDocumentResourceLimit
			}
			reads, writes := storage.reads, storage.writes
			if name == "ready-budget" {
				if _, err := graph.take(context.Background()); !errors.Is(err, want) {
					test.Fatal("ready graph bypassed the current parent budget", err)
				}
			} else if ready, err := graph.step(context.Background()); ready || !errors.Is(err, want) {
				test.Fatal("borrowed graph accepted detached ownership, generation, or stale admission", ready, err)
			}
			if storage.reads != reads || storage.writes != writes {
				test.Fatal("rejected graph performed IO before validating its current parent")
			}
		})
	}
}

func legacyNativeSharedW92RestoreGraph(test *testing.T, request legacyNativeAnyW78Request, source LegacyByteSource, storage *legacyNativeAnyW78CAS) (*legacyNativeObjectCheckpointRestore, *legacyNativeObjectAnyCheckpointRestore, *legacyNativeCheckpointGraphRestore) {
	test.Helper()
	root, err := newLegacyNativeObjectCheckpointRestore(context.Background(), request.Root, legacyNativeCheckpointEventModel, storage.io(), source, request.Binding, legacyNativeEventW82FenceBudget)
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(root.close)
	for attempt := 0; attempt < legacyNativeAnyFieldW84FenceSteps; attempt++ {
		field, ok := root.anyTask.(*legacyNativeObjectAnyCheckpointRestore)
		if ok && field.child != nil && field.child.graph != nil {
			graph, valid := field.child.graph.(*legacyNativeCheckpointGraphRestore)
			if !valid {
				test.Fatal("actual nested graph restore has an unexpected implementation")
			}
			test.Cleanup(field.close)
			return root, field, graph
		}
		if ready, err := root.step(context.Background()); err != nil || ready {
			test.Fatal("actual parent did not expose a live nested graph restore", ready, err)
		}
	}
	test.Fatal("nested graph restore setup exceeded the unchanged finite schedule")
	return nil, nil, nil
}

func TestLegacyNativeObjectCheckpointW92SharedRestoreFences(test *testing.T) {
	bundle, request, storage := legacyNativeSharedW92Fixture(test, false)
	request = legacyNativeEventW82FenceCapture(test, bundle.Object, &bundle.Scanner, request, bundle.Source, storage)
	for _, name := range []string{"budget", "field-detached", "graph-detached", "source-generation", "nested-source-generation", "parent-close", "ready-budget", "text-budget"} {
		test.Run(name, func(test *testing.T) {
			root, field, graph := legacyNativeSharedW92RestoreGraph(test, request, bundle.Source, storage)
			want := ErrConflict
			switch name {
			case "budget":
				root.budget, want = 1, ErrDocumentResourceLimit
			case "field-detached":
				root.anyTask = nil
			case "graph-detached":
				field.child.graph = nil
				test.Cleanup(graph.close)
			case "source-generation":
				field.sources.original.authority++
				want = ErrIntegrity
			case "nested-source-generation":
				field.child.source.authority++
				want = ErrIntegrity
			case "parent-close":
				root.close()
				want = ErrIntegrity
			case "ready-budget":
				legacyNativeAnyFieldW84Ready(test, graph, storage)
				root.budget, want = 1, ErrDocumentResourceLimit
			case "text-budget":
				for attempt := 0; graph.text == nil; attempt++ {
					if attempt >= legacyNativeAnyFieldW84FenceSteps {
						test.Fatal("actual graph did not expose text restore in the finite schedule")
					}
					if ready, err := graph.step(context.Background()); ready || err != nil {
						test.Fatal("actual graph text setup failed", ready, err)
					}
				}
				retained, err := root.retainedBytes()
				if err != nil {
					test.Fatal(err)
				}
				root.budget, want = retained+1, ErrDocumentResourceLimit
			}
			reads, writes := storage.reads, storage.writes
			if name == "ready-budget" {
				if _, err := graph.take(context.Background()); !errors.Is(err, want) {
					test.Fatal("ready graph restore bypassed the current parent budget", err)
				}
			} else if ready, err := graph.step(context.Background()); ready || !errors.Is(err, want) {
				test.Fatal("borrowed graph restore accepted detached ownership, generation, or stale admission", ready, err)
			}
			if storage.reads != reads || storage.writes != writes {
				test.Fatal("rejected graph restore performed IO before validating its current parent")
			}
		})
	}
}
