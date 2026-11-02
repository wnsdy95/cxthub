package domain

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
	"unsafe"
)

const legacyNativeAnyW78Directory = "CXTHUB_W78_NATIVE_ANY_DIRECTORY"
const legacyNativeAnyW78Budget int64 = 128 << 20
const legacyNativeAnyW78StepLimit = 32768

type legacyNativeAnyW78CAS struct {
	directory    string
	reads        int
	writes       int
	quantum      int
	failRead     error
	failPut      error
	inputs       map[ContentHash]int
	inputWrites  int
	dependencies []legacyNativeAnyW78DependencyRead
	restoring    bool
	diagnostic   func() bool
}

type legacyNativeAnyW78DependencyRead struct {
	Hash       ContentHash `json:"hash"`
	Bytes      int         `json:"bytes"`
	Restoring  bool        `json:"restoring"`
	Diagnostic bool        `json:"diagnostic"`
}

func legacyNativeAnyW78Store(test *testing.T) *legacyNativeAnyW78CAS {
	test.Helper()
	return &legacyNativeAnyW78CAS{directory: test.TempDir()}
}

func (storage *legacyNativeAnyW78CAS) objectPath(hash ContentHash) (string, error) {
	if ValidateContentHash(hash) != nil {
		return "", ErrIntegrity
	}
	return filepath.Join(storage.directory, strings.TrimPrefix(string(hash), "sha256:")+".cas"), nil
}

func (storage *legacyNativeAnyW78CAS) sourceManifest(request legacyNativeAnyW78Request) {
	storage.inputs = make(map[ContentHash]int)
	for _, part := range request.Parts {
		storage.inputs[part.Hash] = int(part.Bytes)
	}
}

func (storage *legacyNativeAnyW78CAS) preload(test *testing.T, request legacyNativeAnyW78Request) {
	test.Helper()
	storage.sourceManifest(request)
	for offset := 0; offset < request.SourceBytes; offset += request.PartBytes {
		part := request.Parts[offset/request.PartBytes]
		body := bytes.Clone(request.Suffix[offset : offset+int(part.Bytes)])
		if HashContent(body) != part.Hash {
			test.Fatal("immutable original input part hash changed")
		}
		path, err := storage.objectPath(part.Hash)
		if err != nil {
			test.Fatal(err)
		}
		if previous, err := os.ReadFile(path); err == nil {
			if !bytes.Equal(previous, body) {
				test.Fatal("immutable input CAS collision")
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			test.Fatal(err)
		} else if err := os.WriteFile(path, body, 0600); err != nil {
			test.Fatal(err)
		}
	}
}

func (storage *legacyNativeAnyW78CAS) io() legacyNativeCheckpointIO {
	return legacyNativeCheckpointIO{
		Put: func(ctx context.Context, hash ContentHash, body []byte) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			storage.writes++
			storage.quantum += len(body)
			if _, input := storage.inputs[hash]; input {
				storage.inputWrites++
			}
			if storage.failPut != nil {
				return storage.failPut
			}
			if len(body) == 0 || len(body) > LegacyCanonicalEncodingBytes || HashContent(body) != hash {
				return ErrIntegrity
			}
			path, err := storage.objectPath(hash)
			if err != nil {
				return err
			}
			file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
			if errors.Is(err, os.ErrExist) {
				previous, readErr := os.ReadFile(path)
				if readErr != nil || !bytes.Equal(previous, body) {
					return ErrIntegrity
				}
				return nil
			}
			if err != nil {
				return err
			}
			_, writeErr := file.Write(bytes.Clone(body))
			return errors.Join(writeErr, file.Close())
		},
		Read: func(ctx context.Context, hash ContentHash, length int) ([]byte, error) {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			storage.reads++
			storage.quantum += length
			if storage.failRead != nil {
				return nil, storage.failRead
			}
			if length <= 0 || length > ConversationManifestChunkBytes {
				return nil, ErrIntegrity
			}
			if declared, input := storage.inputs[hash]; input {
				if declared != length {
					return nil, ErrIntegrity
				}
				storage.dependencies = append(storage.dependencies, legacyNativeAnyW78DependencyRead{Hash: hash, Bytes: length, Restoring: storage.restoring, Diagnostic: storage.diagnostic != nil && storage.diagnostic()})
			} else if length > LegacyCanonicalEncodingBytes {
				return nil, ErrIntegrity
			}
			path, err := storage.objectPath(hash)
			if err != nil {
				return nil, err
			}
			return os.ReadFile(path)
		},
	}
}

type legacyNativeAnyW78Work interface {
	step(context.Context) (bool, error)
	retainedBytes() (int64, error)
	close()
}

func legacyNativeAnyW78Drain(test *testing.T, work legacyNativeAnyW78Work, storage *legacyNativeAnyW78CAS, budget int64) int {
	test.Helper()
	for attempt := 1; attempt <= legacyNativeAnyW78StepLimit; attempt++ {
		storage.quantum = 0
		ctx, cancel := context.WithCancel(context.Background())
		ready, err := work.step(ctx)
		cancel()
		retained, heapErr := work.retainedBytes()
		if err != nil || heapErr != nil || retained < 0 || retained > budget || storage.quantum > 512<<10 {
			test.Fatalf("checkpoint step=%d ready=%v retained=%d IO=%d error=%v heap=%v", attempt, ready, retained, storage.quantum, err, heapErr)
		}
		if ready {
			return attempt
		}
	}
	test.Fatal("checkpoint exceeded finite step schedule")
	return 0
}

func legacyNativeAnyW78Released(test *testing.T, work legacyNativeAnyW78Work) {
	test.Helper()
	work.close()
	work.close()
	if retained, err := work.retainedBytes(); err != nil || retained != 0 {
		test.Fatalf("closed checkpoint retained=%d error=%v", retained, err)
	}
}

type legacyNativeAnyW78Request struct {
	Root         legacyNativeCheckpointRef   `json:"root"`
	Binding      ContentHash                 `json:"binding"`
	SourceBytes  int                         `json:"source_bytes"`
	PartBytes    int                         `json:"part_bytes"`
	Parts        []ConversationManifestChunk `json:"parts"`
	SuffixOffset int                         `json:"suffix_offset"`
	Suffix       []byte                      `json:"suffix"`
}

type legacyNativeAnyW78Reads struct {
	Ranges    [][2]int `json:"ranges"`
	Forbidden int      `json:"forbidden"`
}

func legacyNativeAnyW78Source(test *testing.T, request legacyNativeAnyW78Request, reads *legacyNativeAnyW78Reads) LegacyByteSource {
	test.Helper()
	if request.PartBytes != LegacyNormalizationPartBytes && request.PartBytes != ConversationManifestChunkBytes || request.SuffixOffset < 0 || request.SuffixOffset%request.PartBytes != 0 ||
		request.SourceBytes < request.SuffixOffset || len(request.Suffix) != request.SourceBytes-request.SuffixOffset {
		test.Fatal("invalid finite suffix manifest")
	}
	source := LegacyByteSource{Bytes: request.SourceBytes, PartBytes: request.PartBytes, Parts: append([]ConversationManifestChunk(nil), request.Parts...)}
	for offset := request.SuffixOffset; offset < source.Bytes; offset += request.PartBytes {
		ordinal := offset / request.PartBytes
		body := request.Suffix[offset-request.SuffixOffset : min(offset+request.PartBytes, source.Bytes)-request.SuffixOffset]
		if ordinal >= len(source.Parts) || source.Parts[ordinal].Hash != HashContent(body) || source.Parts[ordinal].Bytes != int64(len(body)) {
			test.Fatal("current suffix disagrees with persisted source manifest", ordinal)
		}
		source.Parts[ordinal] = ConversationManifestChunk{Hash: HashContent(body), Bytes: int64(len(body))}
	}
	source.Read = func(ctx context.Context, offset, length int) ([]byte, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		reads.Ranges = append(reads.Ranges, [2]int{offset, length})
		if offset < request.SuffixOffset {
			reads.Forbidden++
			return nil, ErrIntegrity
		}
		if offset > source.Bytes || offset%request.PartBytes != 0 || length != min(request.PartBytes, source.Bytes-offset) {
			return nil, ErrIntegrity
		}
		return bytes.Clone(request.Suffix[offset-request.SuffixOffset : offset-request.SuffixOffset+length]), nil
	}
	prepared, err := NewLegacyNormalizationDecoder(context.Background(), source)
	if err != nil || prepared.source.authority == 0 || len(reads.Ranges) != 0 {
		test.Fatal("fresh current source preparation read RAW or failed", err)
	}
	return prepared.source
}

func legacyNativeAnyW78FullSource(test *testing.T, raw string) (legacyNativeAnyW78Request, LegacyByteSource) {
	return legacyNativeAnyW78FullSourceWidth(test, raw, LegacyNormalizationPartBytes)
}

func legacyNativeAnyW78FullSourceWidth(test *testing.T, raw string, width int) (legacyNativeAnyW78Request, LegacyByteSource) {
	test.Helper()
	request := legacyNativeAnyW78Request{Binding: HashContent([]byte("W78 finite nativeAny job binding")), SourceBytes: len(raw), PartBytes: width, Suffix: []byte(raw)}
	for offset := 0; offset < len(raw); offset += request.PartBytes {
		part := []byte(raw[offset:min(offset+request.PartBytes, len(raw))])
		request.Parts = append(request.Parts, ConversationManifestChunk{Hash: HashContent(part), Bytes: int64(len(part))})
	}
	return request, legacyNativeAnyW78Source(test, request, &legacyNativeAnyW78Reads{})
}

type legacyNativeAnyW78CutContext struct {
	context.Context
	cancel context.CancelFunc
	stop   func() bool
}

func (cut *legacyNativeAnyW78CutContext) Err() error {
	if cut.stop() {
		cut.cancel()
	}
	return cut.Context.Err()
}

type legacyNativeAnyW78Fixture struct {
	name      string
	raw       string
	seed      string
	cut       string
	visible   bool
	partBytes int
}

func (fixture legacyNativeAnyW78Fixture) width() int {
	if fixture.partBytes == 0 {
		return LegacyNormalizationPartBytes
	}
	return fixture.partBytes
}

func legacyNativeAnyW78Fixtures() []legacyNativeAnyW78Fixture {
	width := LegacyNormalizationPartBytes
	pad := func(prefix, ending, suffix string) string {
		return prefix + strings.Repeat(" ", width-len(prefix)-len(ending)) + ending + suffix
	}
	bits := `,"bits":[-0,9007199254740993,5e-324,1.7976931348623157e308]}`
	largeWidth := ConversationManifestChunkBytes
	return []legacyNativeAnyW78Fixture{
		{name: "pending-key", raw: pad(`{`, `"key"`, `:null`+bits), cut: "key"},
		{name: "pending-empty-key", raw: pad(`{`, `""`, `:false`+bits), cut: "key"},
		{name: "pending-null", raw: pad(`{"key":`, `null`, bits), cut: "null"},
		{name: "open-map-array-frames", raw: pad(`{`, `"outer":[{`, `"child":null}],"end":true}`), cut: "frames"},
		{name: "scalar-logical-prefix", raw: `{"text":"` + strings.Repeat("a", 2*width) + "\\n\\uD83D\\uDE42\ud55c\\\"\\\\tail\"" + bits, cut: "scalar"},
		{name: "number-prefix-bound-source", raw: strings.Repeat(" ", width-128) + `[-0.` + strings.Repeat("0", 2*width) + `1,0.1,-0,5e-324]`, cut: "number"},
		{name: "scanner-string-prefix", raw: `{"text":"` + strings.Repeat("b", 2*width) + `\u0041"` + bits, cut: "scanner"},
		{name: "array-growth", raw: pad(`[null,`, `null`, `,{"new":[-0,true]},"tail"]`), cut: "growth"},
		{name: "accepted-opening-run", raw: strings.Repeat(" ", width-128) + strings.Repeat("[", 128) + `null` + strings.Repeat("]", 128), cut: "opening"},
		{name: "accepted-closing-run", raw: pad(`{"nested":`+strings.Repeat("[", 64)+`null`, strings.Repeat("]", 64), `,"tail":true}`), cut: "closing"},
		{name: "first-native-semantic-error", raw: pad(`{"bad":`, `1e99999`, `,"second":2e99999,"ok":[-0,0.1]}`), cut: "semantic"},
		{name: "root-map-reuse", seed: `{"keep":-0,"replace":{"old":true},"":null}`, raw: pad(`{`, `"replace"`, `:{"new":[false,null]},"replace":null,"added":5e-324}`), cut: "key"},
		{name: "separate-visible-owner", seed: `{"previous":[-0,"visible"]}`, raw: pad(`{`, `"current"`, `:[null,true]}`), cut: "key", visible: true},
		{name: "unapplied-small-token-behind-scanner", raw: pad(`{"key":`, ``, `null,"next":[-0,true]}`), cut: "small"},
		{name: "giant-number-future-full-diagnostic", raw: strings.Repeat(" ", width-128) + `[1` + strings.Repeat("0", 2*width) + `,2e99999,-0]`, cut: "number"},
		{name: "giant-number-diagnostic-prefix", raw: strings.Repeat(" ", width-128) + `[1` + strings.Repeat("0", 2*width) + `,2e99999,-0]`, cut: "diagnostic"},
		{name: "giant-number-512KiB-input-dependency", raw: strings.Repeat(" ", largeWidth-128) + `[1` + strings.Repeat("0", 2*largeWidth) + `,2e99999,-0]`, cut: "number", partBytes: largeWidth},
		{name: "giant-number-fresh-partial-boundary", raw: strings.Repeat(" ", width-128) + `[1` + strings.Repeat("0", 2*width) + `,2e99999,-0]`, cut: "number-partial"},
	}
}

func legacyNativeAnyW78Stop(parser *legacyNativeAny, fixture legacyNativeAnyW78Fixture) bool {
	width := fixture.width()
	switch fixture.cut {
	case "key":
		return parser.pending && parser.keyToken && parser.position == width
	case "null":
		return parser.pending && !parser.keyToken && parser.node.value == nil && parser.position == width
	case "frames":
		return parser.depth == 3 && parser.position == width && parser.frame != nil && parser.frame.object != nil && parser.frame.parent.array != nil
	case "scalar":
		return parser.scalar != nil && parser.scalar.position == width && parser.scalar.text != nil && parser.scalar.text.Len() > 0
	case "number":
		return parser.number != nil && parser.number.parser.position == width && parser.number.parser.authority != 0 && !parser.number.converted
	case "number-partial":
		return parser.number != nil && parser.number.parser.position == width+17 && parser.number.parser.authority != 0 && !parser.number.converted
	case "diagnostic":
		return parser.number != nil && parser.number.errorText != nil && parser.number.errorText.position == width && parser.number.rejected && !parser.number.complete
	case "small":
		return parser.applying && parser.scalar == nil && parser.number == nil && parser.tokenStart == width && parser.tokenEnd > width
	case "scanner":
		return parser.scanning && !parser.keyToken && parser.position == width
	case "growth":
		return parser.frame != nil && parser.frame.array != nil && parser.frame.array.previous != nil && parser.position == width
	case "opening":
		return parser.openingRemaining == 128 && parser.position+parser.openingRemaining == width
	case "closing":
		return parser.closingRemaining > 0 && parser.position+parser.closingRemaining == width
	case "semantic":
		return parser.semanticError != nil && parser.pending && !parser.applying && !parser.finished && parser.position == width
	}
	return false
}

func legacyNativeAnyW78Step(test *testing.T, ctx context.Context, parser *legacyNativeAny, scanner *legacyJSONValueScanner, source LegacyByteSource, onePage bool, allowance ...int) (bool, error) {
	test.Helper()
	input := &legacyNormalizationInput{ctx: ctx, source: source}
	if onePage {
		input.loads = 1
	}
	if len(allowance) == 1 {
		input.workBytes = ConversationManifestChunkBytes - allowance[0]
	}
	complete, err := parser.step(ctx, input, scanner)
	if input.failure != nil && !errors.Is(input.failure, context.Canceled) {
		test.Fatal("native parser source failure", input.failure)
	}
	if input.workBytes < 0 || input.workBytes > ConversationManifestChunkBytes || input.loads > 2 || input.unbounded ||
		input.fields < 0 || input.fields > parser.fieldLimit || input.values < 0 || input.values > legacyNativeAnyValueActions {
		test.Fatal("native parser escaped its shared quantum", input.workBytes, input.loads, input.fields, input.values)
	}
	for _, page := range input.pages {
		clear(page)
	}
	return complete, err
}

func legacyNativeAnyW78Continue(test *testing.T, parser *legacyNativeAny, scanner *legacyJSONValueScanner, source LegacyByteSource) int {
	test.Helper()
	for attempt := 1; attempt <= legacyNativeAnyW78StepLimit; attempt++ {
		ctx, cancel := context.WithCancel(context.Background())
		complete, err := legacyNativeAnyW78Step(test, ctx, parser, scanner, source, false)
		cancel()
		var semantic *json.UnmarshalTypeError
		if err != nil && (!complete || !errors.As(err, &semantic)) {
			test.Fatal("native continuation failed", attempt, err)
		}
		if complete {
			if !parser.finished || parser.position != parser.end || parser.frame != nil || parser.depth != 0 {
				test.Fatal("native continuation did not reach exact terminal cursor")
			}
			return attempt
		}
	}
	test.Fatal("native continuation exceeded finite schedule")
	return 0
}

func legacyNativeAnyW78Capture(test *testing.T, fixture legacyNativeAnyW78Fixture, storage *legacyNativeAnyW78CAS) (legacyNativeAnyW78Request, *legacyNativeAny, legacyJSONValueScanner, LegacyByteSource) {
	test.Helper()
	request, source := legacyNativeAnyW78FullSourceWidth(test, fixture.raw, fixture.width())
	storage.preload(test, request)
	parser, err := newLegacyNativeAny(0, len(fixture.raw), 0, 64)
	if err != nil {
		test.Fatal(err)
	}
	var scanner legacyJSONValueScanner
	if fixture.seed != "" {
		_, seedSource := legacyNativeAnyW78FullSource(test, fixture.seed)
		seed, seedErr := newLegacyNativeAny(0, len(fixture.seed), 0, 64)
		if seedErr != nil {
			test.Fatal(seedErr)
		}
		legacyNativeAnyW78Continue(test, seed, &scanner, seedSource)
		if fixture.visible {
			parser.visible = seed.result.owner
		} else {
			parser = seed
			if err := parser.start(0, len(fixture.raw), 0, 64); err != nil {
				test.Fatal(err)
			}
		}
		scanner = legacyJSONValueScanner{}
	}
	for attempt := 0; attempt < legacyNativeAnyW78StepLimit && !legacyNativeAnyW78Stop(parser, fixture); attempt++ {
		base, cancel := context.WithCancel(context.Background())
		cut := &legacyNativeAnyW78CutContext{Context: base, cancel: cancel, stop: func() bool { return legacyNativeAnyW78Stop(parser, fixture) }}
		onePage := (fixture.cut == "number" || fixture.cut == "number-partial" || fixture.cut == "scalar" || fixture.cut == "diagnostic") && parser.applying
		allowance := ConversationManifestChunkBytes
		if fixture.cut == "number-partial" && parser.number != nil && parser.number.parser.position == fixture.width() {
			allowance = 2*int(unsafe.Sizeof(*parser.number.parser)) + 17
		}
		complete, stepErr := legacyNativeAnyW78Step(test, cut, parser, &scanner, source, onePage, allowance)
		cancel()
		if complete || stepErr != nil && !errors.Is(stepErr, context.Canceled) {
			test.Fatal("fixture failed before actual native cut", fixture.name, parser.position, stepErr)
		}
	}
	if !legacyNativeAnyW78Stop(parser, fixture) {
		test.Fatal("fixture never reached advertised native cut", fixture.name, parser.position)
	}
	before := legacyNativeAnyW78State(test, parser, &scanner)
	constructorContext, cancel := context.WithCancel(context.Background())
	work, err := newLegacyNativeAnyCheckpoint(constructorContext, parser, &scanner, request.Binding, request.SourceBytes, storage.io(), legacyNativeAnyW78Budget)
	cancel()
	if err != nil {
		test.Fatal("snapshot construction", err)
	}
	if err := work.bindSource(context.Background(), source); err != nil {
		test.Fatal("snapshot source binding", err)
	}
	legacyNativeAnyW78Drain(test, work, storage, legacyNativeAnyW78Budget)
	request.Root, err = work.take(context.Background())
	if err != nil || !request.Root.valid() {
		test.Fatal("snapshot did not publish valid root", request.Root, err)
	}
	legacyNativeAnyW78Released(test, work)
	if !reflect.DeepEqual(before, legacyNativeAnyW78State(test, parser, &scanner)) {
		test.Fatal("capture mutated borrowed native state")
	}
	request.SuffixOffset = legacyNativeAnyW78SemanticSuffix(parser, fixture.width())
	request.Suffix = bytes.Clone(request.Suffix[request.SuffixOffset:])
	return request, parser, scanner, source
}

func legacyNativeAnyW78SemanticSuffix(parser *legacyNativeAny, partBytes int) int {
	position := parser.position
	if parser.scanning || parser.applying && parser.scalar == nil && parser.number == nil {
		position = min(position, parser.tokenStart)
	}
	if parser.scalar != nil {
		position = min(position, parser.scalar.position)
	}
	if parser.number != nil {
		position = min(position, parser.number.parser.position)
	}
	return position / partBytes * partBytes
}

type legacyNativeAnyW78Semantic struct {
	Present bool   `json:"present"`
	Value   string `json:"value"`
	Type    string `json:"type"`
	Offset  int64  `json:"offset"`
	Struct  string `json:"struct"`
	Field   string `json:"field"`
}

func legacyNativeAnyW78Error(semantic *json.UnmarshalTypeError) legacyNativeAnyW78Semantic {
	if semantic == nil {
		return legacyNativeAnyW78Semantic{}
	}
	typeName := ""
	if semantic.Type != nil {
		typeName = semantic.Type.String()
	}
	return legacyNativeAnyW78Semantic{Present: true, Value: semantic.Value, Type: typeName, Offset: semantic.Offset, Struct: semantic.Struct, Field: semantic.Field}
}

type legacyNativeAnyW78StateRecord struct {
	Control  []int                      `json:"control"`
	Flags    []bool                     `json:"flags"`
	Scanner  []byte                     `json:"scanner"`
	Semantic legacyNativeAnyW78Semantic `json:"semantic"`
	Native   []string                   `json:"native"`
}

func legacyNativeAnyW78Native(test *testing.T, value any) string {
	test.Helper()
	switch typed := value.(type) {
	case nil:
		return "null"
	case bool:
		return fmt.Sprintf("bool:%v", typed)
	case float64:
		return fmt.Sprintf("float:%016x", math.Float64bits(typed))
	case string:
		return "string:" + hex.EncodeToString([]byte(typed))
	case []any:
		if typed == nil {
			return "nil-array"
		}
		var output strings.Builder
		fmt.Fprintf(&output, "array:%d[", len(typed))
		for _, child := range typed {
			output.WriteString(legacyNativeAnyW78Native(test, child) + ";")
		}
		return output.String() + "]"
	case map[string]any:
		if typed == nil {
			return "nil-map"
		}
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		var output strings.Builder
		output.WriteString("map{")
		for _, key := range keys {
			output.WriteString(hex.EncodeToString([]byte(key)) + ":" + legacyNativeAnyW78Native(test, typed[key]) + ";")
		}
		return output.String() + "}"
	default:
		test.Fatalf("unexpected native type %T", value)
		return ""
	}
}

func legacyNativeAnyW78State(test *testing.T, parser *legacyNativeAny, scanner *legacyJSONValueScanner) legacyNativeAnyW78StateRecord {
	test.Helper()
	scannerBytes, err := scanner.checkpoint(context.Background())
	if err != nil {
		test.Fatal(err)
	}
	state := legacyNativeAnyW78StateRecord{
		Control: []int{parser.position, parser.from, parser.end, parser.parentDepth, parser.depth, parser.fieldLimit, parser.tokenStart, parser.tokenEnd, int(parser.tokenFirst), cap(parser.raw), parser.openingRemaining, parser.closingRemaining, int(parser.closingByte)},
		Flags:   []bool{parser.keyToken, parser.scanning, parser.applying, parser.pending, parser.rootReserved, parser.finished, parser.errorContextSet},
		Scanner: scannerBytes, Semantic: legacyNativeAnyW78Error(parser.semanticError),
		Native: []string{legacyNativeAnyW78Native(test, parser.node.value), legacyNativeAnyW78Native(test, parser.result.value)},
	}
	for current := parser.frame; current != nil; current = current.parent {
		state.Control = append(state.Control, int(current.phase), current.keyCapacity)
		state.Native = append(state.Native, "key:"+hex.EncodeToString([]byte(current.key)))
		if current.object != nil {
			state.Native = append(state.Native, legacyNativeAnyW78Native(test, current.object.values()))
			state.Native = append(state.Native, legacyNativeAnyW78OwnerState(test, current.object, nil))
			state.Control = append(state.Control, int(current.object.revision), current.object.dirtyCount)
		}
		if current.array != nil {
			state.Native = append(state.Native, legacyNativeAnyW78Native(test, current.array.values()))
			state.Native = append(state.Native, legacyNativeAnyW78OwnerState(test, nil, current.array))
			state.Control = append(state.Control, cap(current.array.decoded), current.array.maximum, int(current.array.revision))
		}
	}
	if parser.scalar != nil {
		state.Native = append(state.Native, legacyNativeAnyW78ScalarState(test, parser.scalar)...)
	}
	if parser.number != nil {
		number := *parser.number.parser
		number.authority = 0
		decimal := number.decimal
		number.decimal = nil
		state.Native = append(state.Native, fmt.Sprintf("%+v", number), fmt.Sprintf("%+v", decimal), fmt.Sprintf("%+v", legacyNativeAnyW78Error(parser.number.semantic)))
		state.Native = append(state.Native, fmt.Sprintf("number-bits:%016x", math.Float64bits(parser.number.value)))
		state.Flags = append(state.Flags, parser.number.converted, parser.number.rejected, parser.number.complete)
		state.Control = append(state.Control, int(parser.number.diagnostics))
		if parser.number.errorText != nil {
			state.Native = append(state.Native, legacyNativeAnyW78ScalarState(test, parser.number.errorText)...)
		}
	}
	return state
}

func legacyNativeAnyW78ScalarState(test *testing.T, scalar *legacyNativeScalar) []string {
	test.Helper()
	checkpoint, err := scalar.checkpoint(context.Background())
	if err != nil {
		test.Fatal(err)
	}
	defer checkpoint.close()
	encoded, err := json.Marshal(checkpoint.state)
	if err != nil {
		test.Fatal(err)
	}
	state := []string{string(encoded)}
	for offset := 0; offset < checkpoint.state.Bytes; offset += LegacyCanonicalEncodingBytes {
		part, err := checkpoint.read(context.Background(), offset, min(LegacyCanonicalEncodingBytes, checkpoint.state.Bytes-offset))
		if err != nil {
			test.Fatal(err)
		}
		state = append(state, hex.EncodeToString(part))
	}
	return state
}

func legacyNativeAnyW78OwnerState(test *testing.T, object *legacyNativeAnyMap, array *legacyNativeAnyArray) string {
	test.Helper()
	var output strings.Builder
	if object != nil {
		if !object.valid() {
			test.Fatal("invalid native map while observing independent state")
		}
		fmt.Fprintf(&output, "map:%d:%d:%d:%d:%d:%d:", object.revision, object.firstID, object.lastID, object.dirtyHeadID, object.dirtyTailID, object.dirtyCount)
		identity := object.firstID
		for ordinal := 0; identity != 0 && ordinal < object.links.length(); ordinal++ {
			link := object.link(identity)
			if link == nil {
				test.Fatal("missing native map link")
			}
			row := object.ownership[link.canonicalKey]
			fmt.Fprintf(&output, "%s:%d:%d:%d:%d:%d:%d:%v;", hex.EncodeToString([]byte(link.canonicalKey)), row.keyCapacity, row.valueBytes, row.valueExcess, row.linkID, link.nextID, link.dirtyNextID, link.dirty)
			output.WriteString(legacyNativeAnyW78OwnerState(test, row.owner.object, row.owner.array))
			identity = link.nextID
		}
		if identity != 0 {
			test.Fatal("cycle in native map insertion sequence")
		}
	}
	if array != nil {
		fmt.Fprintf(&output, "array:%d:%d:%d:%d:%d:", len(array.values()), cap(array.decoded), array.maximum, array.revision, array.children.length())
		for ordinal := 0; ordinal < array.children.length(); ordinal++ {
			child := array.children.at(ordinal)
			fmt.Fprintf(&output, "%d:%d:%d;", child.capacity, child.heapBytes, child.excess)
			output.WriteString(legacyNativeAnyW78OwnerState(test, child.owner.object, child.owner.array))
		}
	}
	return output.String()
}

func legacyNativeAnyW78HeapAndAliases(test *testing.T, parser *legacyNativeAny) {
	test.Helper()
	var retained docRangeHeaderHeapEstimate
	parser.retained(&retained)
	if retained.err != nil || retained.bytes <= 0 || retained.bytes > legacyNativeAnyW78Budget {
		test.Fatal("restored native retained accounting", retained)
	}
	var live docRangeHeaderHeapEstimate
	var root *legacyNativeAnyFrame
	for current := parser.frame; current != nil; current = current.parent {
		estimate := current.estimate()
		if estimate.err != nil || current.retained != estimate.bytes {
			test.Fatal("frame heap was not recomputed", current.retained, estimate)
		}
		live.include(estimate)
		root = current
		if current.object != nil && !current.object.valid() {
			test.Fatal("restored native map links invalid")
		}
		if current.array != nil && current.array.previous != nil {
			test.Fatal("restored array migration was not normalized")
		}
	}
	if live.err != nil || live.bytes != parser.live.bytes || parser.live.err != nil {
		test.Fatal("restored live heap differs from actual frames", live, parser.live)
	}
	if root != nil && (root.object != parser.rootMap || root.array != parser.rootArray) {
		test.Fatal("root/frame alias split during restore")
	}
	if parser.pending {
		if err := parser.node.validateOwnership(); err != nil {
			test.Fatal("pending owner invalid", err)
		}
	}
	if parser.finished {
		if err := parser.result.validateOwnership(); err != nil {
			test.Fatal("result ownership invalid", err)
		}
		if parser.result.owner.object != parser.rootMap || parser.result.owner.array != parser.rootArray {
			test.Fatal("finished result no longer aliases its root owner")
		}
	}
}

type legacyNativeAnyW78Report struct {
	PID                 int                                `json:"pid"`
	Initial             legacyNativeAnyW78StateRecord      `json:"initial"`
	Final               legacyNativeAnyW78StateRecord      `json:"final"`
	Output              string                             `json:"output"`
	Visible             string                             `json:"visible"`
	RestoreSteps        int                                `json:"restore_steps"`
	ContinuationSteps   int                                `json:"continuation_steps"`
	Reads               legacyNativeAnyW78Reads            `json:"reads"`
	Dependencies        []legacyNativeAnyW78DependencyRead `json:"dependencies"`
	ConsumedNumberBytes int                                `json:"consumed_number_bytes"`
	DiagnosticBytes     int                                `json:"diagnostic_bytes"`
}

func legacyNativeAnyW78JSON(test *testing.T, path string, value any) {
	test.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		test.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0600); err != nil {
		test.Fatal(err)
	}
}

func TestLegacyNativeAnyCheckpointW78ProcessContinuation(test *testing.T) {
	for _, fixture := range legacyNativeAnyW78Fixtures() {
		test.Run(fixture.name, func(test *testing.T) {
			storage := legacyNativeAnyW78Store(test)
			request, original, scanner, source := legacyNativeAnyW78Capture(test, fixture, storage)
			initial := legacyNativeAnyW78State(test, original, &scanner)
			visible := "null"
			if original.visible.object != nil {
				visible = legacyNativeAnyW78Native(test, original.visible.object.values())
			}
			legacyNativeAnyW78Continue(test, original, &scanner, source)
			final := legacyNativeAnyW78State(test, original, &scanner)
			var independent any
			var decodeErr error
			if fixture.seed != "" && !fixture.visible {
				var reused map[string]any
				if err := json.Unmarshal([]byte(fixture.seed), &reused); err != nil {
					test.Fatal(err)
				}
				decodeErr = json.Unmarshal([]byte(fixture.raw), &reused)
				independent = reused
			} else {
				decodeErr = json.Unmarshal([]byte(fixture.raw), &independent)
			}
			var semantic *json.UnmarshalTypeError
			if decodeErr != nil && !errors.As(decodeErr, &semantic) {
				test.Fatal(decodeErr)
			}
			if legacyNativeAnyW78Native(test, original.result.value) != legacyNativeAnyW78Native(test, independent) ||
				legacyNativeAnyW78Error(original.semanticError) != legacyNativeAnyW78Error(semantic) {
				test.Fatal("uninterrupted native result/bits/first error differs from independent JSON decoder")
			}
			legacyNativeAnyW78JSON(test, filepath.Join(storage.directory, "request.json"), request)
			ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestLegacyNativeAnyCheckpointW78ChildHelper$", "-test.count=1", "-test.timeout=40s")
			for _, variable := range os.Environ() {
				if !strings.HasPrefix(variable, legacyNativeAnyW78Directory+"=") {
					command.Env = append(command.Env, variable)
				}
			}
			command.Env = append(command.Env, legacyNativeAnyW78Directory+"="+storage.directory)
			if output, err := command.CombinedOutput(); err != nil {
				test.Fatalf("fresh nativeAny child: %v\n%s", err, output)
			}
			body, err := os.ReadFile(filepath.Join(storage.directory, "report.json"))
			if err != nil {
				test.Fatal(err)
			}
			var report legacyNativeAnyW78Report
			if err := json.Unmarshal(body, &report); err != nil {
				test.Fatal(err)
			}
			if report.PID <= 0 || report.PID == os.Getpid() || report.ContinuationSteps <= 0 || report.RestoreSteps <= 0 ||
				report.Final.Control[0] <= request.SuffixOffset || report.Final.Control[0] <= report.Initial.Control[0] || len(report.Reads.Ranges) == 0 || report.Reads.Forbidden != 0 {
				test.Fatalf("missing fresh-process continuation beyond cursor: %+v", report)
			}
			if !reflect.DeepEqual(report.Initial, initial) || !reflect.DeepEqual(report.Final, final) ||
				report.Output != legacyNativeAnyW78Native(test, independent) || report.Visible != visible {
				test.Fatalf("restored native state/scanner/capacities/error/bits diverged: initial=%v final=%v output=%v visible=%v", reflect.DeepEqual(report.Initial, initial), reflect.DeepEqual(report.Final, final), report.Output == legacyNativeAnyW78Native(test, independent), report.Visible == visible)
			}
			for _, read := range report.Reads.Ranges {
				if read[0] < request.SuffixOffset || read[0]%request.PartBytes != 0 || read[1] != min(request.PartBytes, request.SourceBytes-read[0]) {
					test.Fatal("child read consumed prefix or non-manifest part", read)
				}
			}
			diagnosticReads := 0
			for _, dependency := range report.Dependencies {
				if !dependency.Restoring && !dependency.Diagnostic {
					test.Fatal("original input CAS was used for semantic prefix replay", dependency)
				}
				matched := false
				for ordinal, part := range request.Parts {
					end := ordinal*request.PartBytes + int(part.Bytes)
					if dependency.Hash == part.Hash && dependency.Bytes == int(part.Bytes) && end <= report.ConsumedNumberBytes {
						matched = true
					}
				}
				if !matched {
					test.Fatal("saved source dependency contained an unconsumed part tail", dependency)
				}
				if dependency.Diagnostic {
					diagnosticReads++
				}
				if fixture.cut == "diagnostic" && dependency.Hash == request.Parts[0].Hash {
					test.Fatal("already-emitted diagnostic prefix was re-read instead of scalar checkpoint")
				}
			}
			if strings.Contains(fixture.name, "future-full-diagnostic") || fixture.partBytes == ConversationManifestChunkBytes || fixture.cut == "number-partial" {
				if diagnosticReads == 0 || storage.inputWrites != 0 {
					test.Fatal("Full giant-number cut did not reuse original immutable input refs for new diagnostics", diagnosticReads, storage.inputWrites)
				}
			}
			test.Logf("child PID=%d restored=%d continued=%d suffix-only reads=%d", report.PID, report.RestoreSteps, report.ContinuationSteps, len(report.Reads.Ranges))
		})
	}
}

func TestLegacyNativeAnyCheckpointW78ChildHelper(test *testing.T) {
	directory := os.Getenv(legacyNativeAnyW78Directory)
	if directory == "" {
		return
	}
	body, err := os.ReadFile(filepath.Join(directory, "request.json"))
	if err != nil {
		test.Fatal(err)
	}
	var request legacyNativeAnyW78Request
	if err := json.Unmarshal(body, &request); err != nil {
		test.Fatal(err)
	}
	if request.SourceBytes > 4*ConversationManifestChunkBytes {
		test.Fatal("child request exceeds finite continuation envelope")
	}
	report := legacyNativeAnyW78Report{PID: os.Getpid()}
	source := legacyNativeAnyW78Source(test, request, &report.Reads)
	storage := &legacyNativeAnyW78CAS{directory: directory}
	storage.sourceManifest(request)
	storage.restoring = true
	constructorContext, cancel := context.WithCancel(context.Background())
	work, err := newLegacyNativeAnyCheckpointRestore(constructorContext, request.Root, storage.io(), source, request.Binding, legacyNativeAnyW78Budget)
	cancel()
	if err != nil {
		test.Fatal(err)
	}
	defer work.close()
	report.RestoreSteps = legacyNativeAnyW78Drain(test, work, storage, legacyNativeAnyW78Budget)
	restored, err := work.take(context.Background())
	if err != nil || restored.Parser == nil {
		test.Fatal("child restore take", err)
	}
	if request.SuffixOffset != legacyNativeAnyW78SemanticSuffix(restored.Parser, request.PartBytes) {
		test.Fatal("suffix does not match actual pending semantic position")
	}
	legacyNativeAnyW78Released(test, work)
	if restored.Source.authority != source.authority || restored.Source.Read == nil || restored.Source.Bytes != source.Bytes ||
		restored.Source.PartBytes != source.PartBytes || !reflect.DeepEqual(restored.Source.Parts, source.Parts) {
		test.Fatal("restored Source lost fresh current authority or manifest")
	}
	if report.Reads.Forbidden != 0 {
		test.Fatal("checkpoint restoration replayed consumed RAW source")
	}
	storage.restoring = false
	storage.diagnostic = func() bool {
		return restored.Parser.number != nil && restored.Parser.number.rejected && restored.Parser.number.errorText != nil
	}
	if restored.Parser.number != nil {
		report.ConsumedNumberBytes = restored.Parser.number.parser.position
		if restored.Parser.number.errorText != nil {
			report.DiagnosticBytes = restored.Parser.number.errorText.position
		}
	}
	legacyNativeAnyW78HeapAndAliases(test, restored.Parser)
	if err := restored.Visible.validateOwnership(); err != nil {
		test.Fatal("restored visible payload invalid", err)
	}
	report.Visible = legacyNativeAnyW78Native(test, restored.Visible.value)
	report.Initial = legacyNativeAnyW78State(test, restored.Parser, &restored.Scanner)
	report.ContinuationSteps = legacyNativeAnyW78Continue(test, restored.Parser, &restored.Scanner, restored.Source)
	legacyNativeAnyW78HeapAndAliases(test, restored.Parser)
	report.Final = legacyNativeAnyW78State(test, restored.Parser, &restored.Scanner)
	report.Output = legacyNativeAnyW78Native(test, restored.Parser.result.value)
	report.Dependencies = storage.dependencies
	legacyNativeAnyW78JSON(test, filepath.Join(directory, "report.json"), report)
}

func legacyNativeAnyW78RestoreFailure(test *testing.T, storage *legacyNativeAnyW78CAS, request legacyNativeAnyW78Request, source LegacyByteSource, binding ContentHash, budget int64, io legacyNativeCheckpointIO) {
	test.Helper()
	work, err := newLegacyNativeAnyCheckpointRestore(context.Background(), request.Root, io, source, binding, budget)
	if err != nil {
		if work != nil {
			legacyNativeAnyW78Released(test, work)
		}
		return
	}
	defer work.close()
	for attempt := 0; attempt < legacyNativeAnyW78StepLimit; attempt++ {
		ready, failure := work.step(context.Background())
		if failure != nil {
			if again, sticky := work.step(context.Background()); again || sticky == nil || sticky.Error() != failure.Error() {
				test.Fatal("restore error not sticky", failure, sticky)
			}
			if restored, takeErr := work.take(context.Background()); takeErr == nil || restored.Parser != nil {
				test.Fatal("failed restore exposed native parser")
			}
			if retained, heapErr := work.retainedBytes(); heapErr != nil || retained != 0 {
				test.Fatal("failed restore retained owned state", retained, heapErr)
			}
			legacyNativeAnyW78Released(test, work)
			return
		}
		if ready {
			test.Fatal("invalid checkpoint restored successfully")
		}
	}
	test.Fatal("invalid restore did not terminate")
}

func TestLegacyNativeAnyCheckpointW78Resources(test *testing.T) {
	storage := legacyNativeAnyW78Store(test)
	request, parser, scanner, _ := legacyNativeAnyW78Capture(test, legacyNativeAnyW78Fixtures()[4], storage)
	source := legacyNativeAnyW78Source(test, request, &legacyNativeAnyW78Reads{})
	test.Run("heap-refusal", func(test *testing.T) {
		legacyNativeAnyW78RestoreFailure(test, storage, request, source, request.Binding, 1, storage.io())
		work, err := newLegacyNativeAnyCheckpoint(context.Background(), parser, &scanner, request.Binding, request.SourceBytes, storage.io(), 1)
		if err == nil {
			defer work.close()
			if err := work.bindSource(context.Background(), source); err != nil {
				legacyNativeAnyW78Released(test, work)
				return
			}
			for attempt := 0; attempt < legacyNativeAnyW78StepLimit; attempt++ {
				ready, failure := work.step(context.Background())
				if failure != nil {
					if root, takeErr := work.take(context.Background()); takeErr == nil || root != (legacyNativeCheckpointRef{}) {
						test.Fatal("heap-refused snapshot published root")
					}
					legacyNativeAnyW78Released(test, work)
					return
				}
				if ready {
					test.Fatal("snapshot admitted impossible heap budget")
				}
			}
			test.Fatal("heap refusal did not terminate")
		}
		if work != nil {
			legacyNativeAnyW78Released(test, work)
		}
	})
	for _, kind := range []string{"capture", "restore"} {
		test.Run("cancel-"+kind, func(test *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if kind == "capture" {
				work, err := newLegacyNativeAnyCheckpoint(context.Background(), parser, &scanner, request.Binding, request.SourceBytes, storage.io(), legacyNativeAnyW78Budget)
				if err != nil {
					test.Fatal(err)
				}
				defer work.close()
				if err := work.bindSource(context.Background(), source); err != nil {
					test.Fatal(err)
				}
				if ready, err := work.step(ctx); ready || !errors.Is(err, context.Canceled) {
					test.Fatal("snapshot ignored current cancellation", ready, err)
				}
				if ready, err := work.step(context.Background()); ready || !errors.Is(err, context.Canceled) {
					test.Fatal("snapshot cancellation not sticky", ready, err)
				}
				if root, err := work.take(context.Background()); err == nil || root != (legacyNativeCheckpointRef{}) {
					test.Fatal("canceled snapshot published root")
				}
				if retained, err := work.retainedBytes(); retained != 0 || err != nil {
					test.Fatal("canceled capture retained state", retained, err)
				}
				legacyNativeAnyW78Released(test, work)
			} else {
				work, err := newLegacyNativeAnyCheckpointRestore(context.Background(), request.Root, storage.io(), source, request.Binding, legacyNativeAnyW78Budget)
				if err != nil {
					test.Fatal(err)
				}
				defer work.close()
				if ready, err := work.step(ctx); ready || !errors.Is(err, context.Canceled) {
					test.Fatal("restore ignored current cancellation", ready, err)
				}
				if ready, err := work.step(context.Background()); ready || !errors.Is(err, context.Canceled) {
					test.Fatal("restore cancellation not sticky", ready, err)
				}
				if restored, err := work.take(context.Background()); err == nil || restored.Parser != nil {
					test.Fatal("canceled restore exposed parser")
				}
				if retained, err := work.retainedBytes(); retained != 0 || err != nil {
					test.Fatal("canceled restore retained state", retained, err)
				}
				legacyNativeAnyW78Released(test, work)
			}
		})
	}
	test.Run("capture-put-failure", func(test *testing.T) {
		injected := errors.New("W78 immutable CAS unavailable")
		storage.failPut = injected
		defer func() { storage.failPut = nil }()
		work, err := newLegacyNativeAnyCheckpoint(context.Background(), parser, &scanner, request.Binding, request.SourceBytes, storage.io(), legacyNativeAnyW78Budget)
		if err != nil {
			return
		}
		defer work.close()
		if err := work.bindSource(context.Background(), source); err != nil {
			test.Fatal(err)
		}
		for attempt := 0; attempt < legacyNativeAnyW78StepLimit; attempt++ {
			ready, failure := work.step(context.Background())
			if failure != nil {
				if !errors.Is(failure, injected) {
					test.Fatal(failure)
				}
				if root, err := work.take(context.Background()); err == nil || root != (legacyNativeCheckpointRef{}) {
					test.Fatal("incomplete CAS write published root")
				}
				legacyNativeAnyW78Released(test, work)
				return
			}
			if ready {
				test.Fatal("snapshot completed without CAS")
			}
		}
		test.Fatal("CAS write failure never surfaced")
	})
	for _, kind := range []string{"capture", "restore"} {
		test.Run("cancel-during-IO-"+kind, func(test *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			io := storage.io()
			put, read := io.Put, io.Read
			io.Put = func(current context.Context, hash ContentHash, body []byte) error {
				err := put(current, hash, body)
				cancel()
				return err
			}
			io.Read = func(current context.Context, hash ContentHash, length int) ([]byte, error) {
				body, err := read(current, hash, length)
				cancel()
				return body, err
			}
			var work legacyNativeAnyW78Work
			if kind == "capture" {
				capture, err := newLegacyNativeAnyCheckpoint(context.Background(), parser, &scanner, request.Binding, request.SourceBytes, io, legacyNativeAnyW78Budget)
				if err != nil {
					test.Fatal(err)
				}
				if err := capture.bindSource(context.Background(), source); err != nil {
					test.Fatal(err)
				}
				work = capture
			} else {
				restore, err := newLegacyNativeAnyCheckpointRestore(context.Background(), request.Root, io, source, request.Binding, legacyNativeAnyW78Budget)
				if err != nil {
					test.Fatal(err)
				}
				work = restore
			}
			defer work.close()
			for attempt := 0; attempt < legacyNativeAnyW78StepLimit; attempt++ {
				ready, err := work.step(ctx)
				if err != nil {
					if !errors.Is(err, context.Canceled) {
						test.Fatal("lost in-flight cancellation", err)
					}
					if again, sticky := work.step(context.Background()); again || !errors.Is(sticky, context.Canceled) {
						test.Fatal("in-flight cancellation not sticky", again, sticky)
					}
					if retained, heapErr := work.retainedBytes(); heapErr != nil || retained != 0 {
						test.Fatal("in-flight cancellation leaked native state", retained, heapErr)
					}
					legacyNativeAnyW78Released(test, work)
					return
				}
				if ready {
					test.Fatal("cancellation during immutable IO published readiness")
				}
			}
			test.Fatal("in-flight cancellation did not terminate")
		})
	}
}

func TestLegacyNativeAnyCheckpointW78BindingAndCAS(test *testing.T) {
	storage := legacyNativeAnyW78Store(test)
	request, _, _, _ := legacyNativeAnyW78Capture(test, legacyNativeAnyW78Fixtures()[5], storage)
	source := legacyNativeAnyW78Source(test, request, &legacyNativeAnyW78Reads{})
	reachable := make(map[ContentHash]bool)
	observed := storage.io()
	readObserved := observed.Read
	observed.Read = func(ctx context.Context, hash ContentHash, length int) ([]byte, error) {
		reachable[hash] = true
		return readObserved(ctx, hash, length)
	}
	valid, err := newLegacyNativeAnyCheckpointRestore(context.Background(), request.Root, observed, source, request.Binding, legacyNativeAnyW78Budget)
	if err != nil {
		test.Fatal("valid control before adversarial cases", err)
	}
	legacyNativeAnyW78Drain(test, valid, storage, legacyNativeAnyW78Budget)
	if restored, err := valid.take(context.Background()); err != nil || restored.Parser == nil {
		test.Fatal("valid control did not restore", err)
	}
	legacyNativeAnyW78Released(test, valid)
	test.Run("binding-before-body-load", func(test *testing.T) {
		rootPath, err := storage.objectPath(request.Root.Hash)
		if err != nil {
			test.Fatal(err)
		}
		root, err := os.ReadFile(rootPath)
		if err != nil {
			test.Fatal(err)
		}
		io := storage.io()
		bodyReads := 0
		io.Read = func(ctx context.Context, hash ContentHash, length int) ([]byte, error) {
			if hash != request.Root.Hash {
				bodyReads++
				return nil, errors.New("body access before binding validation")
			}
			return bytes.Clone(root), ctx.Err()
		}
		legacyNativeAnyW78RestoreFailure(test, storage, request, source, HashContent([]byte("wrong job")), legacyNativeAnyW78Budget, io)
		if bodyReads != 0 {
			test.Fatal("binding mismatch read graph bodies", bodyReads)
		}
	})
	for _, name := range []string{"absent-authority", "wrong-source-size", "wrong-consumed-part-hash", "bad-root-hash", "bad-root-length", "zero-root", "read-failure"} {
		test.Run(name, func(test *testing.T) {
			changed, current := request, source
			io := storage.io()
			switch name {
			case "absent-authority":
				current.authority = 0
			case "wrong-source-size":
				current.Bytes--
			case "wrong-consumed-part-hash":
				current.Parts = append([]ConversationManifestChunk(nil), source.Parts...)
				current.Parts[0].Hash = HashContent([]byte("different current consumed part"))
			case "bad-root-hash":
				changed.Root.Hash = HashContent([]byte("unavailable root"))
			case "bad-root-length":
				changed.Root.Bytes++
			case "zero-root":
				changed.Root = legacyNativeCheckpointRef{}
			case "read-failure":
				io.Read = func(context.Context, ContentHash, int) ([]byte, error) { return nil, errors.New("W78 read failed") }
			}
			legacyNativeAnyW78RestoreFailure(test, storage, changed, current, request.Binding, legacyNativeAnyW78Budget, io)
		})
	}
	entries := make([]string, 0, len(reachable))
	for hash := range reachable {
		path, err := storage.objectPath(hash)
		if err != nil {
			test.Fatal(err)
		}
		entries = append(entries, path)
	}
	sort.Strings(entries)
	if len(entries) < 2 {
		test.Fatal("actual checkpoint did not read graph/control bodies", len(entries))
	}
	for ordinal, path := range entries {
		for _, fault := range []string{"missing", "corrupt", "short", "long"} {
			test.Run(fmt.Sprintf("object-%d-%s", ordinal, fault), func(test *testing.T) {
				hash := ContentHash("sha256:" + strings.TrimSuffix(filepath.Base(path), ".cas"))
				io := storage.io()
				read := io.Read
				io.Read = func(ctx context.Context, wanted ContentHash, length int) ([]byte, error) {
					if wanted != hash {
						return read(ctx, wanted, length)
					}
					if fault == "missing" {
						return nil, os.ErrNotExist
					}
					body, err := read(ctx, wanted, length)
					if err != nil || len(body) == 0 {
						return nil, err
					}
					switch fault {
					case "corrupt":
						body[len(body)/2] ^= 1
					case "short":
						body = body[:len(body)-1]
					case "long":
						body = append(body, 0)
					}
					return body, nil
				}
				legacyNativeAnyW78RestoreFailure(test, storage, request, source, request.Binding, legacyNativeAnyW78Budget, io)
			})
		}
	}
	rootBody, err := legacyNativeCheckpointRead(context.Background(), storage.io(), request.Root)
	if err != nil {
		test.Fatal(err)
	}
	unknownField := append(bytes.Clone(rootBody[:len(rootBody)-1]), []byte(`,"unexpected":1}`)...)
	for _, malformed := range [][]byte{append(bytes.Clone(rootBody), '\n'), unknownField, []byte(`{"unknown":1}`), []byte(`null`), []byte(`{}`)} {
		test.Run("canonical-control-root-"+string(HashContent(malformed)), func(test *testing.T) {
			changed := request
			changed.Root, err = legacyNativeCheckpointWrite(context.Background(), storage.io(), malformed)
			if err != nil {
				test.Fatal(err)
			}
			legacyNativeAnyW78RestoreFailure(test, storage, changed, source, request.Binding, legacyNativeAnyW78Budget, storage.io())
		})
	}
	for _, fault := range []string{"missing", "corrupt"} {
		test.Run("future-full-diagnostic-input-"+fault, func(test *testing.T) {
			dependencyStore := legacyNativeAnyW78Store(test)
			pending, _, _, _ := legacyNativeAnyW78Capture(test, legacyNativeAnyW78Fixtures()[14], dependencyStore)
			current := legacyNativeAnyW78Source(test, pending, &legacyNativeAnyW78Reads{})
			io := dependencyStore.io()
			read := io.Read
			io.Read = func(ctx context.Context, hash ContentHash, length int) ([]byte, error) {
				if hash != pending.Parts[0].Hash {
					return read(ctx, hash, length)
				}
				if fault == "missing" {
					return nil, os.ErrNotExist
				}
				body, err := read(ctx, hash, length)
				if err == nil && len(body) > 0 {
					body[len(body)-1] ^= 1
				}
				return body, err
			}
			work, err := newLegacyNativeAnyCheckpointRestore(context.Background(), pending.Root, io, current, pending.Binding, legacyNativeAnyW78Budget)
			if err != nil {
				return
			}
			defer work.close()
			for attempt := 0; attempt < legacyNativeAnyW78StepLimit; attempt++ {
				ready, failure := work.step(context.Background())
				if failure != nil {
					legacyNativeAnyW78Released(test, work)
					return
				}
				if !ready {
					continue
				}
				restored, err := work.take(context.Background())
				if err != nil || restored.Parser == nil || restored.Source.Read == nil {
					test.Fatal("valid dependency metadata could not restore", err)
				}
				legacyNativeAnyW78Released(test, work)
				for step := 0; step < legacyNativeAnyW78StepLimit; step++ {
					input := &legacyNormalizationInput{ctx: context.Background(), source: restored.Source}
					complete, failure := restored.Parser.step(input.ctx, input, &restored.Scanner)
					if failure != nil {
						var semantic *json.UnmarshalTypeError
						if complete || errors.As(failure, &semantic) || restored.Parser.finished {
							test.Fatal("bad immutable input was accepted as semantic result", failure)
						}
						return
					}
					if complete {
						test.Fatal("Full diagnostic trusted missing/corrupt original source dependency")
					}
				}
				test.Fatal("bad dependency continuation did not terminate")
			}
			test.Fatal("bad dependency restore did not terminate")
		})
	}
}

func legacyNativeAnyW78GraphFailure(test *testing.T, storage *legacyNativeAnyW78CAS, saved legacyNativeCheckpointValue) {
	test.Helper()
	registry := newLegacyNativeCheckpointGraphRestored()
	defer registry.close()
	work, err := newLegacyNativeCheckpointGraphRestore(context.Background(), saved, storage.io(), registry, legacyNativeAnyW78Budget)
	if err != nil {
		if work != nil {
			legacyNativeAnyW78Released(test, work)
		}
		return
	}
	defer work.close()
	for attempt := 0; attempt < legacyNativeAnyW78StepLimit; attempt++ {
		ready, failure := work.step(context.Background())
		if failure != nil {
			if again, sticky := work.step(context.Background()); again || sticky == nil || sticky.Error() != failure.Error() {
				test.Fatal("graph rejection not sticky", failure, sticky)
			}
			if value, takeErr := work.take(context.Background()); takeErr == nil || value.value != nil {
				test.Fatal("invalid graph exposed native value")
			}
			legacyNativeAnyW78Released(test, work)
			registry.close()
			if retained, heapErr := registry.retainedBytes(); heapErr != nil || retained != 0 {
				test.Fatal("closed graph registry retained owners", retained, heapErr)
			}
			return
		}
		if ready {
			test.Fatal("invalid graph accepted")
		}
	}
	test.Fatal("invalid graph exceeded finite schedule")
}

func TestLegacyNativeAnyCheckpointW78GraphValidation(test *testing.T) {
	storage := legacyNativeAnyW78Store(test)
	_, source := legacyNativeAnyW78FullSource(test, `{"first":{"child":[null,-0]},"second":{"other":true},"first":{"last":false}}`)
	parser, err := newLegacyNativeAny(0, source.Bytes, 0, 64)
	if err != nil {
		test.Fatal(err)
	}
	var scanner legacyJSONValueScanner
	legacyNativeAnyW78Continue(test, parser, &scanner, source)
	identity := newLegacyNativeCheckpointGraphIdentity()
	defer identity.close()
	writer, err := newLegacyNativeCheckpointGraphWriter(context.Background(), parser.result, storage.io(), identity, legacyNativeAnyW78Budget)
	if err != nil {
		test.Fatal(err)
	}
	defer writer.close()
	legacyNativeAnyW78Drain(test, writer, storage, legacyNativeAnyW78Budget)
	saved, err := writer.take(context.Background())
	if err != nil {
		test.Fatal(err)
	}
	legacyNativeAnyW78Released(test, writer)
	var owner legacyNativeCheckpointOwner
	if err := legacyNativeCheckpointReadJSON(context.Background(), storage.io(), saved.Body, &owner); err != nil {
		test.Fatal(err)
	}
	for _, name := range []string{"unknown-kind", "zero-ID", "out-of-range-ID", "wire-length", "reserved-wire", "owner-ID", "owner-kind", "owner-count", "owner-capacity", "owner-revision", "tree-count", "tree-height", "dirty-duplicate", "dirty-out-of-range", "member-duplicate-key", "conflicting-owner-ID"} {
		test.Run(name, func(test *testing.T) {
			changed := saved
			changed.Wire = bytes.Clone(saved.Wire)
			body := owner
			switch name {
			case "unknown-kind":
				changed.Wire[1] = 255
			case "zero-ID":
				binary.LittleEndian.PutUint64(changed.Wire[16:24], 0)
			case "out-of-range-ID":
				binary.LittleEndian.PutUint64(changed.Wire[16:24], legacyNativeCheckpointNodes+1)
			case "wire-length":
				changed.Wire = changed.Wire[:len(changed.Wire)-1]
			case "reserved-wire":
				changed.Wire[3] = 1
			case "owner-ID":
				body.Identity++
			case "owner-kind":
				body.Kind = legacyNativeCheckpointBool
			case "owner-count":
				body.Length++
			case "owner-capacity":
				body.Capacity = legacyNativeCheckpointNodes + 1
			case "owner-revision":
				body.Revision = 0
			case "tree-count":
				body.Members.Count++
			case "tree-height":
				body.Members.Height = legacyNativeCheckpointTreeHeight + 1
			case "dirty-duplicate", "dirty-out-of-range":
				var dirty legacyNativeCheckpointListWriter[uint64]
				defer dirty.close()
				identities := []uint64{1, 1}
				if name == "dirty-out-of-range" {
					identities = []uint64{body.Length + 1}
				}
				for _, identity := range identities {
					if err := dirty.append(context.Background(), storage.io(), identity); err != nil {
						test.Fatal(err)
					}
				}
				body.Dirty, err = dirty.finish(context.Background(), storage.io())
				if err != nil {
					test.Fatal(err)
				}
			case "member-duplicate-key", "conflicting-owner-ID":
				reader, err := newLegacyNativeCheckpointListReader[legacyNativeCheckpointMember](body.Members, storage.io())
				if err != nil {
					test.Fatal(err)
				}
				defer reader.close()
				first, ok, err := reader.next(context.Background())
				if err != nil || !ok {
					test.Fatal("missing first graph member", err)
				}
				second, ok, err := reader.next(context.Background())
				if err != nil || !ok {
					test.Fatal("missing second graph member", err)
				}
				if name == "member-duplicate-key" {
					second.Key = first.Key
				} else {
					second.Value.Wire = bytes.Clone(second.Value.Wire)
					copy(second.Value.Wire[16:24], first.Value.Wire[16:24])
				}
				var members legacyNativeCheckpointListWriter[legacyNativeCheckpointMember]
				defer members.close()
				for _, member := range []legacyNativeCheckpointMember{first, second} {
					if err := members.append(context.Background(), storage.io(), member); err != nil {
						test.Fatal(err)
					}
				}
				body.Members, err = members.finish(context.Background(), storage.io())
				if err != nil {
					test.Fatal(err)
				}
			}
			if !reflect.DeepEqual(body, owner) {
				changed.Body, err = legacyNativeCheckpointWriteJSON(context.Background(), storage.io(), body)
				if err != nil {
					test.Fatal(err)
				}
			}
			legacyNativeAnyW78GraphFailure(test, storage, changed)
		})
	}
}
