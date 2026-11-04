package domain

import (
	"context"
	"errors"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestLegacyNativeObjectCheckpointW79MetadataRejectBudget(test *testing.T) {
	ctx := context.Background()
	request, source := legacyNativeAnyW78FullSource(test, `{"type":"text"}`)
	store, _ := legacyNativeCheckpointStoreFixture(test)
	ref := legacyNativeCheckpointRef{Hash: HashContent([]byte("opaque checkpoint")), Bytes: 17}
	huge := strings.Repeat("x", 2<<20)
	for _, name := range []string{"ref", "binding", "source", "capture-binding"} {
		test.Run(name, func(test *testing.T) {
			current, changed, binding := source, ref, request.Binding
			if name == "ref" {
				changed.Hash = ContentHash(huge)
			}
			if name == "binding" || name == "capture-binding" {
				binding = ContentHash(huge)
			}
			if name == "source" {
				current.Parts = append([]ConversationManifestChunk(nil), source.Parts...)
				current.Parts[0].Hash = ContentHash(huge)
			}
			native, err := newLegacyNativeObject(reflect.ValueOf(&ContentBlock{}), &legacyContentBlockNativeFields, 1, false)
			if err != nil {
				test.Fatal(err)
			}
			native.start(0, source.Bytes)
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			if name == "capture-binding" {
				var scanner legacyJSONValueScanner
				_, err = newLegacyNativeObjectCheckpoint(ctx, native, &scanner, binding, current, store, 1)
			} else {
				_, err = newLegacyNativeObjectCheckpointRestore(ctx, changed, legacyNativeCheckpointBlockModel, store, current, binding, 1)
			}
			runtime.ReadMemStats(&after)
			if !errors.Is(err, ErrIntegrity) {
				test.Fatalf("oversized external identity not rejected: %v", err)
			}
			if allocated := after.TotalAlloc - before.TotalAlloc; allocated > LegacyCanonicalEncodingBytes {
				test.Fatalf("oversized identity rejection allocated %d bytes before admission", allocated)
			}
		})
	}
}

func legacyNativeObjectW79RetiredNumberSource(test *testing.T) (LegacyByteSource, <-chan struct{}) {
	test.Helper()
	fixture := legacyNativeObjectW79Fixtures()[6]
	fixture.raw = strings.TrimSuffix(fixture.raw, "}") + `,"create_time":null}`
	storage := legacyNativeAnyW78Store(test)
	request, _, _, _ := legacyNativeObjectW79Capture(test, fixture, storage)
	source := legacyNativeAnyW78Source(test, request.Source, &legacyNativeAnyW78Reads{})
	work, err := newLegacyNativeObjectCheckpointRestore(context.Background(), request.Source.Root, request.Model, storage.io(), source, request.Source.Binding, legacyNativeObjectW79Budget)
	if err != nil {
		test.Fatal(err)
	}
	legacyNativeAnyW78Drain(test, work, storage, legacyNativeObjectW79Budget)
	restored, err := work.take(context.Background())
	if err != nil {
		test.Fatal(err)
	}
	retired := restored.Object.typedNumber
	if retired == nil || retired.scalar == nil || retired.scalar.text == nil || retired.scalar.text.Len() == 0 {
		test.Fatal("no real active typed-number prefix")
	}
	released := make(chan struct{})
	runtime.SetFinalizer(retired, func(*legacyNativeTypedNumber) { close(released) })
	if _, err := legacyNativeObjectW79Continue(test, restored.Object, &restored.Scanner, restored.Source, legacyNativeNumberDiagnosticsFull); err != nil {
		test.Fatal(err)
	}
	if restored.Object.typedNumber != nil || restored.Object.destination.Interface().(*ProviderMetadata).CreateTime != nil {
		test.Fatal("later null did not retire number and destination")
	}
	return restored.Source, released
}

func TestLegacyNativeObjectCheckpointW79RetiredNumberLifetime(test *testing.T) {
	source, released := legacyNativeObjectW79RetiredNumberSource(test)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		runtime.GC()
		select {
		case <-released:
			runtime.KeepAlive(source)
			return
		default:
		}
		time.Sleep(5 * time.Millisecond)
	}
	runtime.KeepAlive(source)
	test.Fatal("live Source.Read retained the retired numeric parser/backing after duplicate null")
}
