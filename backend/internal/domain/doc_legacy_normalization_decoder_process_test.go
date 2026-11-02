package domain

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
)

const legacyDecoderCheckpointDirectory = "CXT_DECODER_CHECKPOINT_PROCESS_DIRECTORY"

type legacyDecoderCheckpointProcessRequest struct {
	Root        legacyNativeCheckpointRef   `json:"root"`
	Binding     ContentHash                 `json:"binding"`
	SourceBytes int                         `json:"source_bytes"`
	PartBytes   int                         `json:"part_bytes"`
	Parts       []ConversationManifestChunk `json:"parts"`
	Objects     map[ContentHash]int         `json:"objects"`
}

type legacyDecoderCheckpointProcessReport struct {
	PID          int                       `json:"pid"`
	Restored     []string                  `json:"restored"`
	Recaptured   []string                  `json:"recaptured"`
	Root         legacyNativeCheckpointRef `json:"root"`
	RestoreReads int                       `json:"restore_reads"`
}

func TestLegacyNormalizationDecoderCheckpointNewProcess(test *testing.T) {
	legacyDecoderCheckpointNewProcesses(test, legacyDecoderCheckpointFixtures())
}

func legacyDecoderCheckpointNewProcesses(test *testing.T, fixtures []legacyDecoderCheckpointFixture) {
	test.Helper()
	for _, fixture := range fixtures {
		test.Run(fixture.name, func(test *testing.T) {
			decoder, buffer := legacyDecoderCheckpointCut(test, fixture)
			directory := test.TempDir()
			storage := &legacyNativeAnyW78CAS{directory: directory}
			store := storage.io()
			objects := make(map[ContentHash]int)
			put := store.Put
			store.Put = func(ctx context.Context, hash ContentHash, body []byte) error {
				if err := put(ctx, hash, body); err != nil {
					return err
				}
				objects[hash] = len(body)
				return nil
			}
			binding := HashContent(buffer.body)
			root := legacyDecoderCheckpointCapture(test, decoder, binding, store)
			request := legacyDecoderCheckpointProcessRequest{Root: root, Binding: binding, SourceBytes: decoder.source.Bytes,
				PartBytes: decoder.source.partBytes(), Parts: decoder.source.Parts, Objects: objects}
			if err := os.WriteFile(filepath.Join(directory, "source.json"), buffer.body, 0600); err != nil {
				test.Fatal(err)
			}
			body, err := json.Marshal(request)
			if err != nil {
				test.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(directory, "request.json"), body, 0600); err != nil {
				test.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestLegacyNormalizationDecoderCheckpointProcessChild$", "-test.count=1", "-test.timeout=0")
			command.Env = append(os.Environ(), legacyDecoderCheckpointDirectory+"="+directory)
			output, err := command.CombinedOutput()
			if err != nil {
				test.Fatalf("fresh decoder process failed: %v\n%s", err, output)
			}
			body, err = os.ReadFile(filepath.Join(directory, "report.json"))
			if err != nil {
				test.Fatal(err)
			}
			var report legacyDecoderCheckpointProcessReport
			if err := json.Unmarshal(body, &report); err != nil {
				test.Fatal(err)
			}
			original := legacyDecoderCheckpointContinue(test, decoder)
			if report.PID == os.Getpid() || report.PID <= 0 || report.RestoreReads != 0 || !report.Root.valid() || !reflect.DeepEqual(report.Restored, original) || !reflect.DeepEqual(report.Recaptured, original) {
				test.Fatal("fresh process changed continuation, replayed source, or lost checkpoint closure", report, original)
			}
		})
	}
}

func TestLegacyNormalizationDecoderCheckpointProcessChild(test *testing.T) {
	directory := os.Getenv(legacyDecoderCheckpointDirectory)
	if directory == "" {
		return
	}
	body, err := os.ReadFile(filepath.Join(directory, "request.json"))
	if err != nil {
		test.Fatal(err)
	}
	var request legacyDecoderCheckpointProcessRequest
	if err := json.Unmarshal(body, &request); err != nil || request.SourceBytes <= 0 || request.SourceBytes > 16<<20 || len(request.Parts) > 256 || len(request.Objects) > legacyDecoderCheckpointSteps {
		test.Fatal("process fixture exceeds finite bounds", err)
	}
	file, err := os.Open(filepath.Join(directory, "source.json"))
	if err != nil {
		test.Fatal(err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() != int64(request.SourceBytes) {
		test.Fatal("process source identity differs", err)
	}
	reads := 0
	source := LegacyByteSource{Bytes: request.SourceBytes, PartBytes: request.PartBytes, Parts: request.Parts, Read: func(ctx context.Context, offset, length int) ([]byte, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		reads++
		if offset < 0 || length < 0 || offset > request.SourceBytes || length > request.SourceBytes-offset || length > ConversationManifestChunkBytes {
			return nil, ErrIntegrity
		}
		body := make([]byte, length)
		count, err := file.ReadAt(body, int64(offset))
		if count != length || err != nil && !errors.Is(err, io.EOF) {
			return nil, ErrIntegrity
		}
		return body, nil
	}}
	storage := &legacyNativeAnyW78CAS{directory: directory, restoring: true}
	store := storage.io()
	read, put := store.Read, store.Put
	store.Read = func(ctx context.Context, hash ContentHash, length int) ([]byte, error) {
		if declared, exists := request.Objects[hash]; !exists || declared != length {
			test.Fatal("restore escaped the declared private CAS manifest", hash, length)
		}
		return read(ctx, hash, length)
	}
	store.Put = func(ctx context.Context, hash ContentHash, body []byte) error {
		if err := put(ctx, hash, body); err != nil {
			return err
		}
		request.Objects[hash] = len(body)
		return nil
	}
	restored := legacyDecoderCheckpointRestore(test, request.Root, request.Binding, source, store)
	root := legacyDecoderCheckpointCapture(test, restored, request.Binding, store)
	again := legacyDecoderCheckpointRestore(test, root, request.Binding, source, store)
	if reads != 0 {
		test.Fatal("restore or recapture replayed original source", reads)
	}
	report := legacyDecoderCheckpointProcessReport{PID: os.Getpid(), Root: root, RestoreReads: reads}
	report.Restored = legacyDecoderCheckpointContinue(test, restored)
	report.Recaptured = legacyDecoderCheckpointContinue(test, again)
	body, err = json.Marshal(report)
	if err != nil {
		test.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "report.json"), body, 0600); err != nil {
		test.Fatal(err)
	}
}
