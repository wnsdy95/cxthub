package domain

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

type legacyNormalizationCheckpointImage struct {
	Control  LegacyNormalizationControl
	Header   []byte
	Buffers  [LegacyNormalizationSlots][]byte
	Parts    [LegacyNormalizationSlots][]ConversationManifestChunk
	Physical [LegacyNormalizationSlots][]byte
	Want     ContentHash
}

func TestLegacyNormalizationRestoreHashesBufferedOutput(test *testing.T) {
	physical := bytes.Repeat([]byte("physical"), LegacyCanonicalEncodingBytes/4)
	buffer := bytes.Repeat([]byte("buffer"), LegacyCanonicalEncodingBytes/12)
	logical := append(bytes.Clone(physical), buffer...)
	for _, fixture := range []struct {
		name   string
		offset int
		length int
	}{
		{name: "physical", offset: 13, length: 97},
		{name: "cross-buffer-boundary", offset: len(physical) - 19, length: 71},
		{name: "buffer-only", offset: len(physical) + 17, length: 83},
		{name: "multiple-quanta", offset: 23, length: len(logical) - 23},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			storage := &legacyNormalizationPreparationStorage{test: test}
			storage.slots[0] = bytes.Clone(physical)
			file := legacyNormalizationFile{storage: storage, slot: 0, size: len(logical), buffer: bytes.Clone(buffer)}
			restore := &LegacyNormalizationRestore{work: &LegacyNormalizationPreparation{}}
			defer restore.Close()
			for checked := 0; checked < fixture.length; {
				remaining := fixture.length - checked
				if err := restore.hash(context.Background(), &file, fixture.offset+checked, remaining); err != nil {
					test.Fatal("logical output hash failed", err)
				}
				checked += min(remaining, LegacyCanonicalEncodingBytes)
			}
			digest := sha256.New()
			digest.Write(logical[fixture.offset : fixture.offset+fixture.length])
			want, err := digest.(encoding.BinaryMarshaler).MarshalBinary()
			if err != nil || !bytes.Equal(restore.hashState, want) {
				test.Fatal("restored hash omitted or repeated buffered output", err)
			}
			for _, read := range storage.reads {
				if read.length > LegacyCanonicalEncodingBytes || read.offset+read.length > len(physical) {
					test.Fatal("verification exceeded its physical range or quantum", read)
				}
			}
			if !bytes.Equal(storage.slots[0], physical) || !bytes.Equal(file.buffer, buffer) {
				test.Fatal("verification flushed or changed the saved output")
			}
			before := bytes.Clone(restore.hashState)
			if err := restore.hash(context.Background(), &file, len(logical), 1); !errors.Is(err, ErrPreparedDocRangeObject) ||
				!bytes.Equal(before, restore.hashState) {
				test.Fatal("invalid logical range changed the verified hash", err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if err := restore.hash(ctx, &file, 0, 1); !errors.Is(err, context.Canceled) || !bytes.Equal(before, restore.hashState) {
				test.Fatal("canceled verification changed the hash", err)
			}
		})
	}
}

func legacyNormalizationCheckpointFixture(test *testing.T) []byte {
	test.Helper()
	var document CIRDocument
	if err := json.Unmarshal(legacyNormalizationPreparationMany(2301), &document); err != nil {
		test.Fatal(err)
	}
	document.Envelope.SessionOriginID = strings.Repeat("header", 20000)
	document.Events[0].Kind, document.Events[0].Role = EventMessage, RoleAssistant
	document.Events[0].Blocks = []ContentBlock{{Type: "text", Text: strings.Repeat("large-event🙂", 100000)}}
	raw, err := json.MarshalIndent(document, "", " ")
	if err != nil {
		test.Fatal(err)
	}
	return raw
}

func legacyNormalizationCheckpointTake(test *testing.T, work *LegacyNormalizationPreparation, storage *legacyNormalizationPreparationStorage, want ContentHash) legacyNormalizationCheckpointImage {
	test.Helper()
	ctx := context.Background()
	capture, err := work.BeginCheckpoint(ctx)
	if err != nil {
		test.Fatal(err)
	}
	defer capture.Close()
	control, err := capture.Control(ctx)
	if err != nil {
		test.Fatal(err)
	}
	image := legacyNormalizationCheckpointImage{Control: control, Want: want}
	for offset := 0; offset < control.HeaderBytes; offset += LegacyCanonicalEncodingBytes {
		body, err := capture.ReadHeader(ctx, offset, min(LegacyCanonicalEncodingBytes, control.HeaderBytes-offset))
		if err != nil {
			test.Fatal(err)
		}
		image.Header = append(image.Header, body...)
	}
	for slot, file := range control.Files {
		physical := file.Size - file.BufferBytes
		image.Physical[slot] = bytes.Clone(storage.slots[slot][:physical])
		image.Buffers[slot], err = capture.ReadBuffer(ctx, slot)
		if err != nil {
			test.Fatal(err)
		}
		for first := 0; first < file.Parts; first += 32 {
			parts, err := capture.ReadParts(ctx, slot, first, min(32, file.Parts-first))
			if err != nil {
				test.Fatal(err)
			}
			image.Parts[slot] = append(image.Parts[slot], parts...)
		}
	}
	if _, err := work.Step(ctx); !errors.Is(err, ErrLegacyNormalizationBusy) {
		test.Fatal("checkpoint did not block the actual processing Step", err)
	}
	if _, err := capture.ReadHeader(ctx, 0, LegacyCanonicalEncodingBytes+1); !errors.Is(err, ErrIntegrity) {
		test.Fatal("checkpoint accepted an unbounded header read", err)
	}
	retained, err := capture.RetainedBytes()
	if err != nil || retained <= 0 || retained > 16<<10 {
		test.Fatal("capture copied the entire header/parts/spools", retained, err)
	}
	return image
}

func legacyNormalizationCheckpointRestoreStart(test *testing.T, image legacyNormalizationCheckpointImage) (*LegacyNormalizationRestore, *legacyNormalizationPreparationStorage) {
	test.Helper()
	storage := &legacyNormalizationPreparationStorage{test: test}
	for slot := range storage.slots {
		storage.slots[slot] = bytes.Clone(image.Physical[slot])
	}
	source := LegacyNormalizationCheckpointSource{
		Header: LegacyByteSource{Bytes: len(image.Header), Read: func(ctx context.Context, offset, length int) ([]byte, error) {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if offset < 0 || length < 0 || length > LegacyCanonicalEncodingBytes || offset > len(image.Header) || length > len(image.Header)-offset {
				return nil, ErrIntegrity
			}
			return image.Header[offset : offset+length], nil
		}},
		ReadBuffer: func(ctx context.Context, slot int) ([]byte, error) { return image.Buffers[slot], ctx.Err() },
		ReadParts: func(ctx context.Context, slot, first, count int) ([]ConversationManifestChunk, error) {
			if count > 32 {
				test.Fatal("restore requested unbounded metadata")
			}
			return image.Parts[slot][first : first+count], ctx.Err()
		},
	}
	restore, err := NewLegacyNormalizationRestore(context.Background(), image.Control, source, storage)
	if err != nil {
		test.Fatal(err)
	}
	return restore, storage
}

func legacyNormalizationCheckpointRestore(test *testing.T, image legacyNormalizationCheckpointImage) (*LegacyNormalizationPreparation, *legacyNormalizationPreparationStorage) {
	test.Helper()
	restore, storage := legacyNormalizationCheckpointRestoreStart(test, image)
	defer restore.Close()
	if _, err := restore.Result(); !errors.Is(err, ErrDocRangeIndexPending) {
		test.Fatal("restore exposed preparation before validation", err)
	}
	for quantum := 0; quantum < 100000; quantum++ {
		before := len(storage.reads)
		complete, err := restore.Step(context.Background())
		if err != nil {
			test.Fatal("logical restore failed", quantum, err)
		}
		var readBytes int
		for _, read := range storage.reads[before:] {
			readBytes += read.length
		}
		if readBytes > ConversationManifestChunkBytes {
			test.Fatal("restore performed unbounded physical verification in one Step", readBytes)
		}
		if complete {
			work, err := restore.Result()
			if err != nil || work.decoder != nil || work.pending != nil || work.phase != image.Control.Phase || work.position != image.Control.Position || work.verified != image.Control.Verified {
				test.Fatal("restore replayed input or changed logical progress", err)
			}
			return work, storage
		}
	}
	test.Fatal("logical restore did not finish its bounded input")
	return nil, nil
}

func legacyNormalizationCheckpointFinish(test *testing.T, image legacyNormalizationCheckpointImage) {
	test.Helper()
	work, storage := legacyNormalizationCheckpointRestore(test, image)
	defer work.Close()
	if work.phase != legacyNormalizationReady {
		if _, err := legacyNormalizationPreparationDrain(test, context.Background(), work); err != nil {
			test.Fatal(err)
		}
	}
	if HashContent(storage.slots[3]) != image.Want {
		test.Fatal("resumed actual normalization changed canonical order or bytes")
	}
}

func TestLegacyNormalizationCheckpointActualPhasesInNewProcess(test *testing.T) {
	raw := legacyNormalizationCheckpointFixture(test)
	canonical, _, err := legacyNormalizationPreparationOracle(raw)
	if err != nil {
		test.Fatal(err)
	}
	for _, phase := range []struct {
		name  string
		phase uint8
		after func(*LegacyNormalizationPreparation) bool
	}{
		{name: "runs", phase: legacyNormalizationRuns, after: func(work *LegacyNormalizationPreparation) bool { return work.position > 0 }},
		{name: "merge-pair", phase: legacyNormalizationMerge, after: func(work *LegacyNormalizationPreparation) bool { return work.position > 0 }},
		{name: "prefix", phase: legacyNormalizationPrefix},
		{name: "compose-buffer", phase: legacyNormalizationCompose},
		{name: "compose-event-prefix", phase: legacyNormalizationCompose, after: func(work *LegacyNormalizationPreparation) bool { return work.written > 0 }},
		{name: "suffix", phase: legacyNormalizationSuffix},
		{name: "verified-prefix", phase: legacyNormalizationVerify, after: func(work *LegacyNormalizationPreparation) bool { return work.verified > 0 }},
		{name: "ready-retired-slots", phase: legacyNormalizationReady},
	} {
		test.Run(phase.name, func(test *testing.T) {
			work, storage := legacyNormalizationPreparationStart(test, raw, false)
			defer work.Close()
			reached := func() bool { return work.phase == phase.phase && (phase.after == nil || phase.after(work)) }
			for quantum := 0; !reached(); quantum++ {
				if quantum == 8192 {
					test.Fatal("actual checkpoint phase was never reached")
				}
				if complete, err := work.Step(context.Background()); err != nil || complete && !reached() {
					test.Fatal("normalization passed its requested actual checkpoint phase", err, complete)
				}
			}
			before := append([]legacyNormalizationPreparationRead(nil), storage.reads...)
			image := legacyNormalizationCheckpointTake(test, work, storage, HashContent(canonical))
			if len(storage.reads) != len(before) {
				test.Fatal("logical capture reread or flushed all physical spools")
			}
			if phase.name == "compose-buffer" && image.Control.Files[3].BufferBytes == 0 {
				test.Fatal("fixture did not preserve a nonempty unflushed output buffer")
			}
			if phase.name == "ready-retired-slots" && image.Control.Files[0].Size != 0 {
				test.Fatal("Ready checkpoint retained unused event data")
			}
			path := filepath.Join(test.TempDir(), "logical-state.json")
			encoded, err := json.Marshal(image)
			if err != nil {
				test.Fatal(err)
			}
			if err := os.WriteFile(path, encoded, 0600); err != nil {
				test.Fatal(err)
			}
			work.Close()
			command := exec.Command(os.Args[0], "-test.run=^TestLegacyNormalizationCheckpointProcessChild$", "-test.count=1", "-test.timeout=0")
			command.Env = append(os.Environ(), "CXT_NORMALIZATION_LOGICAL_CHECKPOINT="+path)
			if output, err := command.CombinedOutput(); err != nil {
				test.Fatalf("new-process logical normalization failed: %v\n%s", err, output)
			}
		})
	}
}

func TestLegacyNormalizationCheckpointProcessChild(test *testing.T) {
	path := os.Getenv("CXT_NORMALIZATION_LOGICAL_CHECKPOINT")
	if path == "" {
		test.Skip("only the actual-phase parent launches this helper")
	}
	body, err := os.ReadFile(path)
	if err != nil {
		test.Fatal(err)
	}
	var image legacyNormalizationCheckpointImage
	if err := json.Unmarshal(body, &image); err != nil {
		test.Fatal(err)
	}
	legacyNormalizationCheckpointFinish(test, image)
}

func TestLegacyNormalizationCheckpointCallbackFences(test *testing.T) {
	for _, action := range []string{"close restore", "reenter restore"} {
		test.Run(action, func(test *testing.T) {
			work, storage := legacyNormalizationPreparationStart(test, legacyNormalizationPreparationMany(33), false)
			defer work.Close()
			legacyNormalizationPreparationAdvance(test, work, func() bool { return work.phase == legacyNormalizationRuns })
			image := legacyNormalizationCheckpointTake(test, work, storage, "")
			restore, _ := legacyNormalizationCheckpointRestoreStart(test, image)
			defer restore.Close()
			read := restore.source.ReadParts
			entered := false
			restore.source.ReadParts = func(ctx context.Context, slot, first, count int) ([]ConversationManifestChunk, error) {
				if !entered {
					entered = true
					if action == "close restore" {
						restore.Close()
					} else {
						if _, err := restore.Step(ctx); !errors.Is(err, ErrLegacyNormalizationBusy) {
							test.Error("metadata callback reentered restore", err)
						}
						if _, err := restore.Result(); !errors.Is(err, ErrLegacyNormalizationBusy) {
							test.Error("metadata callback could transfer restore ownership", err)
						}
					}
				}
				return read(ctx, slot, first, count)
			}
			_, err := restore.Step(context.Background())
			if action == "close restore" && !errors.Is(err, ErrConflict) || action == "reenter restore" && err != nil {
				test.Fatal("metadata callback bypassed restore lifetime", err)
			}
		})
	}
	work, _ := legacyNormalizationPreparationStart(test, legacyNormalizationPreparationMany(33), false)
	defer work.Close()
	legacyNormalizationPreparationAdvance(test, work, func() bool { return work.phase == legacyNormalizationPrefix })
	header := bytes.Clone(work.header)
	work.header, work.headerSource = nil, &LegacyByteSource{Bytes: len(header), Read: func(ctx context.Context, offset, length int) ([]byte, error) {
		work.Close()
		return header[offset : offset+length], ctx.Err()
	}}
	if _, err := work.Step(context.Background()); !errors.Is(err, ErrConflict) || work.files[3].size != len(canonicalDocPrefix) || work.segment != 1 || work.segmentPos != 0 {
		test.Fatal("header callback advanced a closed normalization scope", err, work.files[3].size, work.segment, work.segmentPos)
	}
}

func TestLegacyNormalizationCheckpointRejectsCursorDamage(test *testing.T) {
	for _, phase := range []uint8{legacyNormalizationRuns, legacyNormalizationCompose} {
		work, storage := legacyNormalizationPreparationStart(test, legacyNormalizationPreparationMany(33), false)
		legacyNormalizationPreparationAdvance(test, work, func() bool { return work.phase == phase })
		image := legacyNormalizationCheckpointTake(test, work, storage, "")
		work.Close()
		if phase == legacyNormalizationRuns {
			image.Control.SegmentPos = len(canonicalDocPrefix) + 1
		} else {
			image.Control.Position = 1
		}
		if err := image.Control.Validate(); !errors.Is(err, ErrIntegrity) {
			test.Fatal("checkpoint accepted a future prefix cursor or skipped output event", phase, err)
		}
	}
}

func TestLegacyNormalizationCheckpointHeaderCannotReenterPreparation(test *testing.T) {
	work, storage := legacyNormalizationPreparationStart(test, legacyNormalizationPreparationMany(33), false)
	defer work.Close()
	legacyNormalizationPreparationAdvance(test, work, func() bool { return work.phase == legacyNormalizationPrefix })
	header := bytes.Clone(work.header)
	called := false
	work.header, work.headerSource = nil, &LegacyByteSource{Bytes: len(header), Read: func(ctx context.Context, offset, length int) ([]byte, error) {
		if !called {
			called = true
			if _, err := work.Step(ctx); !errors.Is(err, ErrLegacyNormalizationBusy) {
				test.Error("header callback reentered its processing Step", err)
			}
		}
		return header[offset : offset+length], ctx.Err()
	}}
	if _, err := work.Step(context.Background()); err != nil || !called || work.phase != legacyNormalizationCompose || work.segment != 3 || work.segmentPos != 0 {
		test.Fatal("header source corrupted prefix progress", err, work.phase, work.segment, work.segmentPos)
	}
	want := append(append([]byte(canonicalDocPrefix), header...), []byte(canonicalDocMiddle)...)
	output := append(bytes.Clone(storage.slots[3]), work.files[3].buffer...)
	if !bytes.Equal(output, want) || work.prefixBytes != len(want) {
		test.Fatal("header was appended twice after callback reentry")
	}
}

func TestLegacyNormalizationCheckpointRestoreRejectsPhysicalDamage(test *testing.T) {
	work, storage := legacyNormalizationPreparationStart(test, legacyNormalizationCheckpointFixture(test), false)
	defer work.Close()
	legacyNormalizationPreparationAdvance(test, work, func() bool { return work.phase == legacyNormalizationCompose && work.written > 0 })
	composing := legacyNormalizationCheckpointTake(test, work, storage, "")
	legacyNormalizationPreparationAdvance(test, work, func() bool { return work.phase == legacyNormalizationVerify && work.verified > 0 })
	verifying := legacyNormalizationCheckpointTake(test, work, storage, "")
	for _, damage := range []struct {
		name   string
		image  legacyNormalizationCheckpointImage
		slot   int
		offset int
	}{
		{name: "partial-page-hash", image: composing, slot: 3, offset: composing.Control.Files[3].PartsBytes},
		{name: "active-event-hash", image: composing, slot: 0, offset: int(composing.Control.Active.Offset)},
		{name: "active-order-record", image: composing, slot: composing.Control.SourceSlot, offset: composing.Control.Position * legacyNormalizationRecordBytes},
		{name: "already-verified-prefix", image: verifying, slot: 3},
	} {
		test.Run(damage.name, func(test *testing.T) {
			image := damage.image
			image.Physical[damage.slot] = bytes.Clone(image.Physical[damage.slot])
			image.Physical[damage.slot][damage.offset] ^= 1
			restore, _ := legacyNormalizationCheckpointRestoreStart(test, image)
			defer restore.Close()
			for quantum := 0; quantum < 100000; quantum++ {
				complete, err := restore.Step(context.Background())
				if err != nil {
					if !errors.Is(err, ErrPreparedDocRangeObject) {
						test.Fatal("damaged restored bytes changed their error category", err)
					}
					if _, resultErr := restore.Result(); !errors.Is(resultErr, ErrPreparedDocRangeObject) {
						test.Fatal("failed restore exposed usable preparation", resultErr)
					}
					return
				}
				if complete {
					test.Fatal("physical damage bypassed the saved page/event/verified cursor")
				}
			}
			test.Fatal("damaged restore did not terminate")
		})
	}
}
