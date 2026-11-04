package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

type legacyNormalizationParserImage struct {
	State         legacyNormalizationCheckpointImage
	Ref           LegacyNormalizationParserCheckpointRef
	Objects       map[ContentHash][]byte
	Raw           []byte
	PartBytes     int
	DecoderCount  int
	DecoderPhase  uint8
	DecoderOffset int
}

func legacyNormalizationParserCut(test *testing.T, fixture legacyDecoderCheckpointFixture, partBytes int) (*LegacyNormalizationPreparation, *legacyNormalizationPreparationStorage, *legacySourceProofBuffer) {
	test.Helper()
	buffer, source := legacyInputGeometrySource([]byte(fixture.raw), partBytes)
	storage := &legacyNormalizationPreparationStorage{test: test}
	work, err := NewLegacyNormalizationPreparation(context.Background(), source, storage)
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(work.Close)
	for range legacyDecoderCheckpointSteps {
		if work.phase == legacyNormalizationDecode && fixture.cut(work.decoder) {
			return work, storage, buffer
		}
		if complete, err := work.Step(context.Background()); complete || err != nil {
			test.Fatal("preparation ended before the actual parser boundary", fixture.name, complete, err)
		}
	}
	test.Fatal("actual preparation never reached parser boundary", fixture.name)
	return nil, nil, nil
}

func legacyNormalizationParserCapture(test *testing.T, work *LegacyNormalizationPreparation, storage *legacyNormalizationPreparationStorage, buffer *legacySourceProofBuffer) legacyNormalizationParserImage {
	test.Helper()
	ctx := context.Background()
	capture, err := work.BeginCheckpoint(ctx)
	if err != nil {
		test.Fatal("actual Decode capture failed", err)
	}
	defer capture.Close()
	store, objects := legacyNativeCheckpointStoreFixture(test)
	io := LegacyNormalizationParserCheckpointIO{Put: store.Put, Read: store.Read}
	reads := len(buffer.reads)
	var root LegacyNormalizationParserCheckpointRef
	for range legacyDecoderCheckpointSteps {
		ref, complete, err := capture.ParserStep(ctx, HashContent(buffer.body), io, legacyDecoderCheckpointBudget)
		if err != nil {
			test.Fatal("parser bridge failed", err)
		}
		if _, err := work.Step(ctx); !errors.Is(err, ErrLegacyNormalizationBusy) {
			test.Fatal("parser bridge released the shared preparation freeze", err)
		}
		retained, err := capture.RetainedBytes()
		workBytes, workErr := work.RetainedBytes()
		if err != nil || workErr != nil || retained < 0 || retained+workBytes > legacyDecoderCheckpointBudget {
			test.Fatal("capture exceeded its shared budget", retained, workBytes, err, workErr)
		}
		if complete {
			root = ref
			break
		}
	}
	if root == (LegacyNormalizationParserCheckpointRef{}) || len(buffer.reads) != reads {
		test.Fatal("parser capture failed to finish without source replay", root, len(buffer.reads)-reads)
	}
	again, complete, err := capture.ParserStep(ctx, HashContent(buffer.body), io, legacyDecoderCheckpointBudget)
	if err != nil || !complete || again != root {
		test.Fatal("completed capture did not retain its parser root", err)
	}
	want, _, err := legacyNormalizationPreparationOracle(buffer.body)
	if err != nil {
		test.Fatal(err)
	}
	return legacyNormalizationParserImage{State: legacyNormalizationCheckpointTake(test, work, storage, HashContent(want)), Ref: root,
		Objects: objects, Raw: bytes.Clone(buffer.body), PartBytes: work.decoder.source.partBytes(),
		DecoderCount: work.decoder.count, DecoderPhase: work.decoder.phase, DecoderOffset: work.decoder.position}
}

func legacyNormalizationParserRestoreStart(test *testing.T, image legacyNormalizationParserImage, binding ContentHash) (*LegacyNormalizationRestore, *legacyNormalizationPreparationStorage, *legacySourceProofBuffer) {
	test.Helper()
	buffer, input := legacyInputGeometrySource(image.Raw, image.PartBytes)
	input.Read = func(context.Context, int, int) ([]byte, error) {
		test.Error("restore read original input instead of checkpoint objects")
		return nil, ErrIntegrity
	}
	storage := &legacyNormalizationPreparationStorage{test: test}
	for slot := range storage.slots {
		storage.slots[slot] = bytes.Clone(image.State.Physical[slot])
	}
	store, objects := legacyNativeCheckpointStoreFixture(test)
	for hash, body := range image.Objects {
		objects[hash] = bytes.Clone(body)
	}
	source := LegacyNormalizationCheckpointSource{
		ReadBuffer: func(ctx context.Context, slot int) ([]byte, error) {
			return image.State.Buffers[slot], ctx.Err()
		},
		ReadParts: func(ctx context.Context, slot, first, count int) ([]ConversationManifestChunk, error) {
			if count > 32 {
				test.Fatal("unbounded spool metadata restore")
			}
			return image.State.Parts[slot][first : first+count], ctx.Err()
		},
		Parser: &LegacyNormalizationParserCheckpointSource{Ref: image.Ref, Binding: binding, Source: input,
			Store: LegacyNormalizationParserCheckpointIO{Put: store.Put, Read: store.Read}, Budget: legacyDecoderCheckpointBudget},
	}
	restore, err := NewLegacyNormalizationRestore(context.Background(), image.State.Control, source, storage)
	if err != nil {
		test.Fatal("parser restore construction failed", err)
	}
	return restore, storage, buffer
}

func legacyNormalizationParserFinish(test *testing.T, image legacyNormalizationParserImage) {
	test.Helper()
	restore, storage, buffer := legacyNormalizationParserRestoreStart(test, image, HashContent(image.Raw))
	defer restore.Close()
	if _, err := restore.Result(); !errors.Is(err, ErrDocRangeIndexPending) {
		test.Fatal("restore exposed unvalidated parser state", err)
	}
	for range legacyDecoderCheckpointSteps {
		complete, err := restore.Step(context.Background())
		if err != nil {
			test.Fatal("parser restore failed", err)
		}
		retained, err := restore.RetainedBytes()
		if err != nil || retained <= 0 || retained > legacyDecoderCheckpointBudget {
			test.Fatal("restore escaped its total budget", retained, err)
		}
		if !complete {
			continue
		}
		work, err := restore.Result()
		if err != nil {
			test.Fatal(err)
		}
		defer work.Close()
		if work.decoder == nil || work.mutation != work.decoder.mutation || work.phase != legacyNormalizationDecode || work.pending != nil ||
			work.count != image.State.Control.Count || work.decoder.count != image.DecoderCount || work.decoder.position != image.DecoderOffset || work.decoder.phase != image.DecoderPhase || len(buffer.reads) != 0 {
			test.Fatal("restore changed the actual parser cursor, count, authority ownership, or phase")
		}
		work.decoder.source.Read = buffer.Read
		complete = false
		for range legacyDecoderCheckpointSteps {
			var err error
			complete, err = work.Step(context.Background())
			if err != nil {
				test.Fatal("restored actual preparation failed to finish", err)
			}
			retained, err := work.RetainedBytes()
			if err != nil || retained < 0 || retained > legacyDecoderCheckpointBudget {
				test.Fatal("restored large parser exceeded its admitted budget", retained, err)
			}
			if complete {
				break
			}
		}
		if !complete {
			test.Fatal("restored actual preparation did not finish")
		}
		if HashContent(storage.slots[3]) != image.State.Want {
			test.Fatal("resumed preparation changed canonical bytes or event order")
		}
		return
	}
	test.Fatal("parser restore did not finish")
}

func TestLegacyNormalizationParserCheckpointActualPreparationInNewProcess(test *testing.T) {
	fixtures := []legacyDecoderCheckpointFixture{legacyDecoderCheckpointFixtures()[0], legacyDecoderCheckpointFixtures()[1], legacyHeaderCheckpointFixtures()[0]}
	for index, fixture := range fixtures {
		partBytes := LegacyNormalizationPartBytes
		if index == 0 {
			partBytes = ConversationManifestChunkBytes
		}
		test.Run(fmt.Sprintf("%s/%d", fixture.name, partBytes), func(test *testing.T) {
			work, storage, buffer := legacyNormalizationParserCut(test, fixture, partBytes)
			image := legacyNormalizationParserCapture(test, work, storage, buffer)
			if image.State.Control.Version != 2 || !image.State.Control.Parsing() || image.State.Control.HeaderBytes != 0 {
				test.Fatal("Decode bridge did not produce the logical v2 boundary")
			}
			if fixture.name == "active-event-child-scalar" && (image.State.Control.Count != 1 || image.State.Control.Files[1].BufferBytes == 0 || image.State.Control.Files[0].BufferBytes == 0) {
				test.Fatal("fixture missed staged events and open spool prefixes")
			}
			body, err := json.Marshal(image)
			if err != nil {
				test.Fatal(err)
			}
			path := filepath.Join(test.TempDir(), "parser-state.json")
			if err := os.WriteFile(path, body, 0600); err != nil {
				test.Fatal(err)
			}
			work.Close()
			command := exec.Command(os.Args[0], "-test.run=^TestLegacyNormalizationParserCheckpointProcessChild$", "-test.count=1", "-test.timeout=0")
			command.Env = append(os.Environ(), "CXT_NORMALIZATION_PARSER_BRIDGE="+path)
			if output, err := command.CombinedOutput(); err != nil {
				test.Fatalf("new-process preparation restore failed: %v\n%s", err, output)
			}
		})
	}
}

func TestLegacyNormalizationParserCheckpointProcessChild(test *testing.T) {
	path := os.Getenv("CXT_NORMALIZATION_PARSER_BRIDGE")
	if path == "" {
		test.Skip("only the actual preparation parent launches this helper")
	}
	body, err := os.ReadFile(path)
	if err != nil {
		test.Fatal(err)
	}
	var image legacyNormalizationParserImage
	if err := json.Unmarshal(body, &image); err != nil {
		test.Fatal(err)
	}
	legacyNormalizationParserFinish(test, image)
}

func TestLegacyNormalizationParserCheckpointBoundariesAndCompatibility(test *testing.T) {
	work, _ := legacyNormalizationPreparationStart(test, legacyNormalizationPreparationMany(2), false)
	defer work.Close()
	legacyNormalizationPreparationAdvance(test, work, func() bool { return work.phase == legacyNormalizationStage })
	stage, err := work.BeginCheckpoint(context.Background())
	if err != nil {
		test.Fatal("initial Stage batch must preserve its real pending events", err)
	}
	staged, err := stage.Control(context.Background())
	stage.Close()
	if err != nil || !staged.Staging() || staged.Version != 3 || work.mutation.activity.Load() != 0 {
		test.Fatal("Stage capture changed its version or retained mutation ownership", staged, err)
	}
	legacyNormalizationPreparationAdvance(test, work, func() bool { return work.phase == legacyNormalizationRuns })
	capture, err := work.BeginCheckpoint(context.Background())
	if err != nil {
		test.Fatal(err)
	}
	defer capture.Close()
	control, err := capture.Control(context.Background())
	if err != nil || control.Version != 1 || control.Parsing() || control.Validate() != nil {
		test.Fatal("post-decoder v1 contract changed", err)
	}
	if ref, complete, err := capture.ParserStep(context.Background(), "", LegacyNormalizationParserCheckpointIO{}, 0); err != nil || !complete || ref != (LegacyNormalizationParserCheckpointRef{}) {
		test.Fatal("v1 capture unexpectedly required a parser codec", ref, complete, err)
	}
	if _, err := NewLegacyNormalizationRestore(context.Background(), control, LegacyNormalizationCheckpointSource{
		Header: LegacyByteSource{Bytes: control.HeaderBytes, Read: func(context.Context, int, int) ([]byte, error) { return nil, ErrIntegrity }},
		Parser: &LegacyNormalizationParserCheckpointSource{},
	}, &legacyNormalizationPreparationStorage{test: test}); !errors.Is(err, ErrIntegrity) {
		test.Fatal("v1 control accepted an unexpected parser reference", err)
	}
}

func TestLegacyNormalizationParserCheckpointControlGeometry(test *testing.T) {
	control := LegacyNormalizationControl{Version: 2, Phase: legacyNormalizationDecode}
	for slot := range control.Files {
		control.Files[slot].Generation = 1
	}
	if err := control.Validate(); err != nil {
		test.Fatal("initial Decode boundary rejected", err)
	}
	for _, change := range []func(*LegacyNormalizationControl){
		func(state *LegacyNormalizationControl) { state.Version = 1 },
		func(state *LegacyNormalizationControl) { state.Phase = legacyNormalizationStage },
		func(state *LegacyNormalizationControl) { state.Phase = legacyNormalizationRuns },
		func(state *LegacyNormalizationControl) { state.Count = 1 },
		func(state *LegacyNormalizationControl) { state.HeaderBytes = 1 },
		func(state *LegacyNormalizationControl) { state.PrefixBytes = 1 },
		func(state *LegacyNormalizationControl) { state.Written = 1 },
		func(state *LegacyNormalizationControl) { state.HashState = []byte{1} },
		func(state *LegacyNormalizationControl) { state.Active.Length = 1 },
		func(state *LegacyNormalizationControl) { state.SourceSlot = 1 },
		func(state *LegacyNormalizationControl) { state.TargetSlot = 2 },
		func(state *LegacyNormalizationControl) { state.Width = 1 },
		func(state *LegacyNormalizationControl) { state.Base = 1 },
		func(state *LegacyNormalizationControl) { state.Left = 1 },
		func(state *LegacyNormalizationControl) { state.Right = 1 },
		func(state *LegacyNormalizationControl) { state.Segment = 1 },
		func(state *LegacyNormalizationControl) { state.SegmentPos = 1 },
		func(state *LegacyNormalizationControl) { state.Verified = 1 },
		func(state *LegacyNormalizationControl) { state.Files[2].Size, state.Files[2].BufferBytes = 1, 1 },
		func(state *LegacyNormalizationControl) { state.Files[3].Size, state.Files[3].BufferBytes = 1, 1 },
	} {
		changed := control
		change(&changed)
		if !errors.Is(changed.Validate(), ErrIntegrity) {
			test.Fatalf("invalid Decode geometry accepted: %+v", changed)
		}
	}
	for _, scenario := range []struct {
		decoded int
		stored  int
		invalid bool
		valid   bool
	}{{3, 3, false, true}, {3, 2, true, true}, {3, 2, false, false}, {2, 3, true, false}} {
		decoder := &LegacyNormalizationDecoder{count: scenario.decoded, invalid: scenario.invalid}
		if legacyNormalizationParserCount(decoder, scenario.stored) != scenario.valid {
			test.Fatal("parser count check lost the failed-preparation distinction", scenario)
		}
	}
}

func TestLegacyNormalizationParserCheckpointFences(test *testing.T) {
	fixture := legacyDecoderCheckpointFixtures()[0]
	for _, action := range []string{"cancel", "close-during-write", "write-failure", "budget", "reenter"} {
		test.Run(action, func(test *testing.T) {
			work, _, buffer := legacyNormalizationParserCut(test, fixture, LegacyNormalizationPartBytes)
			capture, err := work.BeginCheckpoint(context.Background())
			if err != nil {
				test.Fatal(err)
			}
			defer capture.Close()
			store, _ := legacyNativeCheckpointStoreFixture(test)
			io := LegacyNormalizationParserCheckpointIO{Put: store.Put, Read: store.Read}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			entered := false
			io.Put = func(ctx context.Context, hash ContentHash, body []byte) error {
				entered = true
				if _, err := work.Step(ctx); !errors.Is(err, ErrLegacyNormalizationBusy) {
					test.Fatal("write callback could mutate preparation", err)
				}
				switch action {
				case "cancel":
					cancel()
				case "close-during-write":
					capture.Close()
				case "write-failure":
					return ErrNotFound
				case "reenter":
					if _, _, err := capture.ParserStep(ctx, hash, LegacyNormalizationParserCheckpointIO{}, legacyDecoderCheckpointBudget); !errors.Is(err, ErrLegacyNormalizationBusy) {
						test.Fatal("write callback reentered capture", err)
					}
				}
				return store.Put(ctx, hash, body)
			}
			budget := legacyDecoderCheckpointBudget
			if action == "budget" {
				budget = 1
			}
			var failure error
			complete := false
			for range legacyDecoderCheckpointSteps {
				_, complete, failure = capture.ParserStep(ctx, HashContent(buffer.body), io, budget)
				if failure != nil || complete {
					break
				}
			}
			want := map[string]error{"cancel": context.Canceled, "close-during-write": ErrConflict, "write-failure": ErrNotFound, "budget": ErrDocumentResourceLimit}[action]
			if action == "reenter" {
				if failure != nil || !complete || !entered {
					test.Fatal("bounded capture failed after denying reentry", complete, failure)
				}
				capture.Close()
			} else if !errors.Is(failure, want) || action == "budget" && entered || action != "budget" && !entered {
				test.Fatal("capture failure or admission gate changed", entered, failure, want)
			}
			if work.mutation.activity.Load() != 0 || work.mutation.capture.Load() != nil {
				test.Fatal("failed or closed capture retained the shared mutation grant")
			}
		})
	}
}

func TestLegacyNormalizationParserCheckpointRejectsBindingAndCount(test *testing.T) {
	work, storage, buffer := legacyNormalizationParserCut(test, legacyDecoderCheckpointFixtures()[1], LegacyNormalizationPartBytes)
	image := legacyNormalizationParserCapture(test, work, storage, buffer)
	for _, action := range []string{"binding", "count", "cancel"} {
		test.Run(action, func(test *testing.T) {
			changed := image
			binding := HashContent(image.Raw)
			if action == "binding" {
				binding = HashContent([]byte("another source binding"))
			}
			if action == "count" {
				var control legacyNormalizationDecoderControl
				if err := json.Unmarshal(image.Objects[image.Ref.Hash], &control); err != nil {
					test.Fatal(err)
				}
				control.Count++
				body, err := json.Marshal(control)
				if err != nil {
					test.Fatal(err)
				}
				changed.Objects = make(map[ContentHash][]byte, len(image.Objects)+1)
				for hash, original := range image.Objects {
					changed.Objects[hash] = original
				}
				changed.Ref = LegacyNormalizationParserCheckpointRef{Hash: HashContent(body), Bytes: len(body)}
				changed.Objects[changed.Ref.Hash] = body
			}
			restore, _, _ := legacyNormalizationParserRestoreStart(test, changed, binding)
			defer restore.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var failure error
			for range legacyDecoderCheckpointSteps {
				if action == "cancel" && restore.parser != nil && restore.parser.child != nil {
					cancel()
				}
				complete, err := restore.Step(ctx)
				if err != nil {
					failure = err
					break
				}
				if complete {
					test.Fatal("restore accepted a mismatched or cancelled parser")
				}
			}
			want := error(ErrIntegrity)
			if action == "cancel" {
				want = context.Canceled
			}
			if !errors.Is(failure, want) || restore.parser != nil {
				test.Fatal("restore failed to reject and release parser ownership", failure, want)
			}
		})
	}
}

func TestLegacyNormalizationParserCheckpointBoundedInput(test *testing.T) {
	work, _, buffer := legacyNormalizationParserCut(test, legacyDecoderCheckpointFixtures()[0], LegacyNormalizationPartBytes)
	capture, err := work.BeginCheckpoint(context.Background())
	if err != nil {
		test.Fatal(err)
	}
	defer capture.Close()
	size, partBytes, err := capture.ParserInputGeometry(context.Background())
	if err != nil || size != len(buffer.body) || partBytes != LegacyNormalizationPartBytes {
		test.Fatal("parser input geometry changed", size, partBytes, err)
	}
	if _, err := capture.ReadParserInput(context.Background(), 0, LegacyCanonicalEncodingBytes+1); !errors.Is(err, ErrIntegrity) {
		test.Fatal("input staging allowed an unbounded read", err)
	}
	body, err := capture.ReadParserInput(context.Background(), 0, LegacyCanonicalEncodingBytes)
	if err != nil || !bytes.Equal(body, buffer.body[:LegacyCanonicalEncodingBytes]) {
		test.Fatal("bounded input staging failed", err)
	}
	body[0] ^= 1
	if body[0] == buffer.body[0] {
		test.Fatal("input staging exposed borrowed source bytes")
	}
	read := work.decoder.source.Read
	work.decoder.source.Read = func(ctx context.Context, offset, length int) ([]byte, error) {
		capture.Close()
		return read(ctx, offset, length)
	}
	if _, err := capture.ReadParserInput(context.Background(), 0, 1); !errors.Is(err, ErrConflict) {
		test.Fatal("input read ignored capture revocation during IO", err)
	}
}
