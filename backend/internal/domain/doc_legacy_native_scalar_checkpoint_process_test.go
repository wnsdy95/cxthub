package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const legacyNativeScalarCheckpointProcessMarker = "CXTHUB_W72_SCALAR_CHECKPOINT_CHILD"
const legacyNativeScalarCheckpointProcessInput = "CXTHUB_W72_SCALAR_CHECKPOINT_INPUT"
const legacyNativeScalarCheckpointProcessBudget int64 = 64 << 20

type legacyNativeScalarCheckpointProcessRequest struct {
	State        legacyNativeScalarCheckpoint `json:"state"`
	PrefixWidth  int                          `json:"prefix_width"`
	PrefixParts  []ConversationManifestChunk  `json:"prefix_parts"`
	PrefixBodies [][]byte                     `json:"prefix_bodies"`
	SourceBytes  int                          `json:"source_bytes"`
	SourceParts  []ConversationManifestChunk  `json:"source_parts"`
	Suffix       []byte                       `json:"suffix"`
	ExpectedHash ContentHash                  `json:"expected_hash"`
}

type legacyNativeScalarCheckpointProcessReport struct {
	ProcessID         int         `json:"process_id"`
	Hash              ContentHash `json:"hash"`
	Text              string      `json:"text"`
	Position          int         `json:"position"`
	CopyBytes         int         `json:"copy_bytes"`
	MinimumCopySteps  int         `json:"minimum_copy_steps"`
	RestoreSteps      int         `json:"restore_steps"`
	ContinuationSteps int         `json:"continuation_steps"`
	PrefixReads       [][2]int    `json:"prefix_reads"`
	RawReads          [][2]int    `json:"raw_reads"`
	ForbiddenRawReads int         `json:"forbidden_raw_reads"`
}

type legacyNativeScalarCheckpointProcessStore struct {
	parts    [][]byte
	width    int
	reads    [][2]int
	contexts []context.Context
	borrowed []byte
	failure  error
	delta    int
	cancel   context.CancelFunc
}

func (storage *legacyNativeScalarCheckpointProcessStore) read(ctx context.Context, offset, length int) ([]byte, error) {
	storage.reads = append(storage.reads, [2]int{offset, length})
	storage.contexts = append(storage.contexts, ctx)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if storage.failure != nil {
		return nil, storage.failure
	}
	if offset < 0 || storage.width <= 0 || offset%storage.width != 0 || offset/storage.width >= len(storage.parts) {
		return nil, ErrIntegrity
	}
	body := storage.parts[offset/storage.width]
	if length != len(body) || length+storage.delta < 0 {
		return nil, ErrIntegrity
	}
	storage.borrowed = make([]byte, length+storage.delta)
	copy(storage.borrowed, body)
	if storage.cancel != nil {
		storage.cancel()
	}
	return storage.borrowed, nil
}

func legacyNativeScalarCheckpointProcessSource(body []byte, partBytes int) (LegacyByteSource, *legacyNativeScalarCheckpointProcessStore) {
	width := partBytes
	if width == 0 {
		width = ConversationManifestChunkBytes
	}
	storage := &legacyNativeScalarCheckpointProcessStore{width: width}
	source := LegacyByteSource{Bytes: len(body), PartBytes: partBytes, Read: storage.read}
	for offset := 0; offset < len(body); offset += width {
		part := bytes.Clone(body[offset:min(offset+width, len(body))])
		storage.parts = append(storage.parts, part)
		source.Parts = append(source.Parts, ConversationManifestChunk{Hash: HashContent(part), Bytes: int64(len(part))})
	}
	return source, storage
}

func legacyNativeScalarCheckpointProcessDrain(test *testing.T, ctx context.Context, work *legacyNativeScalarRestore) int {
	test.Helper()
	for attempt := 1; attempt <= 64; attempt++ {
		ready, err := work.step(ctx)
		if err != nil {
			test.Fatal(err)
		}
		retained, err := work.retainedBytes()
		if err != nil || retained < 0 || retained > legacyNativeScalarCheckpointProcessBudget {
			test.Fatalf("restore retained bytes=%d error=%v", retained, err)
		}
		if ready {
			return attempt
		}
	}
	test.Fatal("restore exceeded finite step bound")
	return 0
}

func TestLegacyNativeScalarCheckpointProcessContinuation(test *testing.T) {
	cut := LegacyNormalizationPartBytes
	fixtures := []struct {
		name         string
		rawPrefix    []byte
		logicalSeed  string
		logicalText  string
		suffix       string
		suffixText   string
		prefixWidth  int
		prefixWork   int
		wantBuffered int
	}{
		{
			name:      "ascii_then_escaped_unicode",
			rawPrefix: bytes.Repeat([]byte("a"), cut), logicalText: strings.Repeat("a", cut),
			suffix: "\\n\\uD83D\\uDE42\ud55c\\\"\\\\tail", suffixText: "\n🙂\ud55c\"\\tail",
			prefixWidth: LegacyNormalizationPartBytes, prefixWork: 2 * cut,
		},
		{
			name:      "numeric_diagnostic_logical_offset",
			rawPrefix: bytes.Repeat([]byte("9"), cut), logicalSeed: "number ", logicalText: strings.Repeat("9", cut),
			suffix: "e99999", suffixText: "e99999",
			prefixWidth: 0, prefixWork: 2 * cut,
		},
		{
			name:      "expanded_utf8_pending_suffix",
			rawPrefix: bytes.Repeat([]byte{0xff}, cut), logicalText: strings.Repeat("�", cut),
			suffix: "\\u0041\\n\ud55c\\uD83D\\uDE42" + "\xfftail", suffixText: "A\n\ud55c🙂�tail",
			prefixWidth: LegacyNormalizationPartBytes, prefixWork: 4*cut + 11, wantBuffered: 11,
		},
	}
	for _, fixture := range fixtures {
		test.Run(fixture.name, func(test *testing.T) {
			ctx := context.Background()
			raw := append(bytes.Clone(fixture.rawPrefix), []byte(fixture.suffix)...)
			source, _ := legacyNativeScalarCheckpointProcessSource(raw, LegacyNormalizationPartBytes)
			maximum := 3*len(raw) + len(fixture.logicalSeed)
			if fixture.logicalSeed != "" {
				maximum = len(raw) + len(fixture.logicalSeed)
			}
			builder := &strings.Builder{}
			builder.Grow(maximum)
			_, _ = builder.WriteString(fixture.logicalSeed)
			original := &legacyNativeScalar{text: builder, end: len(raw), maximum: maximum}
			input := &legacyNormalizationInput{ctx: ctx, source: source, workBytes: ConversationManifestChunkBytes - fixture.prefixWork}
			complete, err := original.step(ctx, input)
			accepted := fixture.logicalSeed + fixture.logicalText
			if err != nil || complete || original.position != cut || original.text.String() != accepted || original.buffered != fixture.wantBuffered {
				test.Fatalf("prefix position=%d bytes=%d buffered=%d complete=%v error=%v", original.position, original.text.Len(), original.buffered, complete, err)
			}
			if fixture.wantBuffered > 0 && !original.expanded {
				test.Fatal("replacement fixture did not enter expanded native text state")
			}
			before := *original
			snapshot, err := original.checkpoint(ctx)
			if err != nil {
				test.Fatal(err)
			}
			if *original != before || original.text.String() != accepted {
				test.Fatal("checkpoint mutated the live scalar")
			}
			prefix := make([]byte, 0, snapshot.state.Bytes)
			for offset := 0; offset < snapshot.state.Bytes; offset += LegacyCanonicalEncodingBytes {
				part, err := snapshot.read(ctx, offset, min(LegacyCanonicalEncodingBytes, snapshot.state.Bytes-offset))
				if err != nil || len(part) > LegacyCanonicalEncodingBytes {
					test.Fatalf("bounded snapshot fragment bytes=%d error=%v", len(part), err)
				}
				prefix = append(prefix, part...)
			}
			if string(prefix) != accepted {
				test.Fatal("snapshot did not contain exactly the accepted logical prefix")
			}
			prefixSource, prefixStorage := legacyNativeScalarCheckpointProcessSource(prefix, fixture.prefixWidth)
			expected := accepted + fixture.suffixText
			request := legacyNativeScalarCheckpointProcessRequest{
				State: snapshot.state, PrefixWidth: fixture.prefixWidth,
				PrefixParts: prefixSource.Parts, PrefixBodies: prefixStorage.parts,
				SourceBytes: len(raw), SourceParts: source.Parts, Suffix: bytes.Clone(raw[cut:]),
				ExpectedHash: HashContent([]byte(expected)),
			}
			snapshot.close()
			if request.State.Position != cut || cut%LegacyNormalizationPartBytes != 0 || len(request.Suffix) != request.SourceBytes-cut {
				test.Fatal("child wire did not preserve an aligned absolute current position")
			}
			encoded, err := json.Marshal(request)
			if err != nil {
				test.Fatal(err)
			}
			inputPath := filepath.Join(test.TempDir(), "scalar-continuation.json")
			if err := os.WriteFile(inputPath, encoded, 0600); err != nil {
				test.Fatal(err)
			}
			command := exec.Command(os.Args[0], "-test.run=^TestLegacyNativeScalarCheckpointProcessChild$", "-test.count=1", "-test.timeout=0")
			for _, variable := range os.Environ() {
				if !strings.HasPrefix(variable, legacyNativeScalarCheckpointProcessMarker+"=") && !strings.HasPrefix(variable, legacyNativeScalarCheckpointProcessInput+"=") {
					command.Env = append(command.Env, variable)
				}
			}
			command.Env = append(command.Env, legacyNativeScalarCheckpointProcessMarker+"=1", legacyNativeScalarCheckpointProcessInput+"="+inputPath)
			if output, err := command.CombinedOutput(); err != nil {
				test.Fatalf("scalar child failed: %v\n%s", err, output)
			}
			encodedReport, err := os.ReadFile(inputPath + ".report")
			if err != nil {
				test.Fatal(err)
			}
			var report legacyNativeScalarCheckpointProcessReport
			if err := json.Unmarshal(encodedReport, &report); err != nil {
				test.Fatal(err)
			}
			if report.ProcessID <= 0 || report.ProcessID == os.Getpid() || report.Text != expected || report.Hash != request.ExpectedHash || report.Position != request.State.End {
				test.Fatalf("child pid=%d hash=%s position=%d text bytes=%d, want hash=%s position=%d text bytes=%d", report.ProcessID, report.Hash, report.Position, len(report.Text), request.ExpectedHash, request.State.End, len(expected))
			}
			minimumCopies := (request.State.Bytes + LegacyCanonicalEncodingBytes - 1) / LegacyCanonicalEncodingBytes
			if report.CopyBytes != request.State.Bytes || report.MinimumCopySteps != minimumCopies || report.RestoreSteps < minimumCopies+1 || len(report.PrefixReads) != len(request.PrefixParts) || len(report.RawReads) == 0 || report.ForbiddenRawReads != 0 {
				test.Fatalf("child copy bytes=%d minimum copy steps=%d restore steps=%d prefix reads=%d raw reads=%d forbidden raw reads=%d", report.CopyBytes, report.MinimumCopySteps, report.RestoreSteps, len(report.PrefixReads), len(report.RawReads), report.ForbiddenRawReads)
			}
			for ordinal, read := range report.PrefixReads {
				if read != [2]int{ordinal * prefixStorage.width, int(request.PrefixParts[ordinal].Bytes)} {
					test.Fatalf("prefix read=%v ordinal=%d", read, ordinal)
				}
			}
			for _, read := range report.RawReads {
				if read[0] < cut || read[0]%LegacyNormalizationPartBytes != 0 || read[1] != min(LegacyNormalizationPartBytes, request.SourceBytes-read[0]) {
					test.Fatalf("child read unavailable raw prefix or unauthenticated range: %v", read)
				}
			}
			test.Logf("pid=%d hash=%s prefix reads=%d raw reads=%d restored copy bytes=%d restore steps=%d minimum copy steps=%d continuation steps=%d", report.ProcessID, report.Hash, len(report.PrefixReads), len(report.RawReads), report.CopyBytes, report.RestoreSteps, report.MinimumCopySteps, report.ContinuationSteps)
		})
	}
	test.Log("Finite page-aligned scalar fixtures only; numeric text seeds the diagnostic prefix rather than restoring its enclosing number parser. CopyBytes observes accepted builder length at take; MinimumCopySteps is a contract-derived lower bound, not per-step copy instrumentation. No contiguous-allocation/RSS bound, raw-source authority, or enclosing worker checkpoint is established.")
}

func TestLegacyNativeScalarCheckpointProcessChild(test *testing.T) {
	if os.Getenv(legacyNativeScalarCheckpointProcessMarker) != "1" {
		return
	}
	inputPath := os.Getenv(legacyNativeScalarCheckpointProcessInput)
	encoded, err := os.ReadFile(inputPath)
	if err != nil {
		test.Fatal(err)
	}
	var request legacyNativeScalarCheckpointProcessRequest
	if err := json.Unmarshal(encoded, &request); err != nil {
		test.Fatal(err)
	}
	if request.State.Position != LegacyNormalizationPartBytes || request.SourceBytes > 2*LegacyNormalizationPartBytes || request.SourceBytes < request.State.Position || len(request.Suffix) != request.SourceBytes-request.State.Position || request.State.Bytes > 4*LegacyNormalizationPartBytes || len(request.PrefixBodies) != len(request.PrefixParts) {
		test.Fatal("child request exceeds finite aligned fixture bounds")
	}
	width := request.PrefixWidth
	if width == 0 {
		width = ConversationManifestChunkBytes
	}
	storage := &legacyNativeScalarCheckpointProcessStore{parts: request.PrefixBodies, width: width}
	prefix := LegacyByteSource{Bytes: request.State.Bytes, PartBytes: request.PrefixWidth, Parts: request.PrefixParts, Read: storage.read}
	report := legacyNativeScalarCheckpointProcessReport{ProcessID: os.Getpid()}
	ctx := context.Background()
	rawSource := LegacyByteSource{Bytes: request.SourceBytes, PartBytes: LegacyNormalizationPartBytes, Parts: request.SourceParts}
	rawSource.Read = func(current context.Context, offset, length int) ([]byte, error) {
		report.RawReads = append(report.RawReads, [2]int{offset, length})
		if err := current.Err(); err != nil {
			return nil, err
		}
		if offset < request.State.Position {
			report.ForbiddenRawReads++
			return nil, ErrIntegrity
		}
		if offset > request.SourceBytes || length < 0 || length > request.SourceBytes-offset {
			return nil, ErrIntegrity
		}
		return bytes.Clone(request.Suffix[offset-request.State.Position : offset-request.State.Position+length]), nil
	}
	work, err := newLegacyNativeScalarRestore(ctx, request.State, prefix, request.SourceBytes, legacyNativeScalarCheckpointProcessBudget)
	if err != nil {
		test.Fatal(err)
	}
	defer work.close()
	if len(storage.reads) != 0 || len(report.RawReads) != 0 {
		test.Fatal("restore constructor performed source IO")
	}
	ready := false
	for report.RestoreSteps < 64 && !ready {
		beforeReads := len(storage.reads)
		ready, err = work.step(ctx)
		report.RestoreSteps++
		if err != nil || len(storage.reads)-beforeReads > 1 || len(report.RawReads) != 0 {
			test.Fatalf("restore step=%d prefix reads=%d raw reads=%d error=%v", report.RestoreSteps, len(storage.reads)-beforeReads, len(report.RawReads), err)
		}
		if report.RestoreSteps == 1 && (ready || len(storage.reads) != 0) {
			test.Fatal("workspace allocation was not a separate restore step")
		}
		retained, err := work.retainedBytes()
		if err != nil || retained < 0 || retained > legacyNativeScalarCheckpointProcessBudget {
			test.Fatalf("restore retained bytes=%d error=%v", retained, err)
		}
	}
	if !ready {
		test.Fatal("child restore exceeded finite step bound")
	}
	scalar, err := work.take(ctx)
	if err != nil || scalar == nil || scalar.text == nil || scalar.text.Len() != request.State.Bytes || scalar.position != request.State.Position || scalar.end != request.State.End || scalar.previous != nil || scalar.buffered != 0 || scalar.copied != 0 || scalar.copyDeferred {
		test.Fatalf("restored scalar lost logical state or retained inactive cursors: %v", err)
	}
	report.CopyBytes = scalar.text.Len()
	report.MinimumCopySteps = (report.CopyBytes + LegacyCanonicalEncodingBytes - 1) / LegacyCanonicalEncodingBytes
	report.PrefixReads = storage.reads
	work.close()
	complete := false
	for report.ContinuationSteps < 32 && !complete {
		input := &legacyNormalizationInput{ctx: ctx, source: rawSource}
		complete, err = scalar.step(ctx, input)
		report.ContinuationSteps++
		if err != nil || input.loads > 2 || input.workBytes > ConversationManifestChunkBytes {
			test.Fatalf("continuation position=%d work=%d error=%v", scalar.position, input.workBytes, err)
		}
	}
	if !complete || scalar.position != request.State.End || scalar.buffered != 0 {
		test.Fatal("scalar did not reach the exact original raw end")
	}
	report.Text, report.Position = scalar.text.String(), scalar.position
	report.Hash = HashContent([]byte(report.Text))
	if report.Hash != request.ExpectedHash {
		test.Fatalf("continued text hash=%s, want %s", report.Hash, request.ExpectedHash)
	}
	input := &legacyNormalizationInput{ctx: ctx, source: rawSource}
	readCount := len(report.RawReads)
	complete, err = scalar.step(ctx, input)
	if err != nil || !complete || len(report.RawReads) != readCount || scalar.text.String() != report.Text || scalar.position != report.Position {
		test.Fatal("completed scalar reread raw input or changed its text")
	}
	encodedReport, err := json.Marshal(report)
	if err != nil {
		test.Fatal(err)
	}
	if err := os.WriteFile(inputPath+".report", encodedReport, 0600); err != nil {
		test.Fatal(err)
	}
}

func TestLegacyNativeScalarCheckpointProcessFences(test *testing.T) {
	ctx := context.Background()
	payload := bytes.Repeat([]byte("p"), 2*LegacyCanonicalEncodingBytes+17)
	newFixture := func(partBytes int) (legacyNativeScalarCheckpoint, LegacyByteSource, *legacyNativeScalarCheckpointProcessStore, int) {
		prefix, storage := legacyNativeScalarCheckpointProcessSource(payload, partBytes)
		sourceBytes := LegacyNormalizationPartBytes + 31
		state := legacyNativeScalarCheckpoint{
			Version: 1, Position: LegacyNormalizationPartBytes, End: sourceBytes,
			Maximum: 3 * sourceBytes, Bytes: len(payload), Capacity: len(payload) + 128, Present: true,
		}
		return state, prefix, storage, sourceBytes
	}
	test.Run("metadata_and_admission_before_io", func(test *testing.T) {
		fixtures := []struct {
			name   string
			mutate func(*legacyNativeScalarCheckpoint, *LegacyByteSource, *int)
		}{
			{"version_zero", func(state *legacyNativeScalarCheckpoint, source *LegacyByteSource, extent *int) { state.Version = 0 }},
			{"version_future", func(state *legacyNativeScalarCheckpoint, source *LegacyByteSource, extent *int) { state.Version = 2 }},
			{"negative_position", func(state *legacyNativeScalarCheckpoint, source *LegacyByteSource, extent *int) { state.Position = -1 }},
			{"reversed_source_range", func(state *legacyNativeScalarCheckpoint, source *LegacyByteSource, extent *int) {
				state.End = state.Position - 1
			}},
			{"end_past_source", func(state *legacyNativeScalarCheckpoint, source *LegacyByteSource, extent *int) {
				state.End = *extent + 1
			}},
			{"negative_source_extent", func(state *legacyNativeScalarCheckpoint, source *LegacyByteSource, extent *int) { *extent = -1 }},
			{"source_extent_over_limit", func(state *legacyNativeScalarCheckpoint, source *LegacyByteSource, extent *int) {
				*extent = MaxFinalizedDocBytes + 1
			}},
			{"negative_logical_bytes", func(state *legacyNativeScalarCheckpoint, source *LegacyByteSource, extent *int) { state.Bytes = -1 }},
			{"bytes_over_maximum", func(state *legacyNativeScalarCheckpoint, source *LegacyByteSource, extent *int) {
				state.Maximum = state.Bytes - 1
			}},
			{"maximum_over_global_limit", func(state *legacyNativeScalarCheckpoint, source *LegacyByteSource, extent *int) {
				state.Maximum = 6*MaxFinalizedDocBytes + 4096
			}},
			{"capacity_below_bytes", func(state *legacyNativeScalarCheckpoint, source *LegacyByteSource, extent *int) {
				state.Capacity = state.Bytes - 1
			}},
			{"capacity_over_slack", func(state *legacyNativeScalarCheckpoint, source *LegacyByteSource, extent *int) {
				state.Capacity = 2*state.Maximum + 65
			}},
			{"absent_with_text", func(state *legacyNativeScalarCheckpoint, source *LegacyByteSource, extent *int) {
				state.Present = false
			}},
			{"prefix_extent_mismatch", func(state *legacyNativeScalarCheckpoint, source *LegacyByteSource, extent *int) { source.Bytes-- }},
			{"unsupported_part_width", func(state *legacyNativeScalarCheckpoint, source *LegacyByteSource, extent *int) { source.PartBytes = 1 }},
			{"missing_part", func(state *legacyNativeScalarCheckpoint, source *LegacyByteSource, extent *int) {
				source.Parts = source.Parts[:len(source.Parts)-1]
			}},
			{"extra_part", func(state *legacyNativeScalarCheckpoint, source *LegacyByteSource, extent *int) {
				source.Parts = append(source.Parts, source.Parts[0])
			}},
			{"short_descriptor", func(state *legacyNativeScalarCheckpoint, source *LegacyByteSource, extent *int) {
				source.Parts[0].Bytes--
			}},
			{"long_tail_descriptor", func(state *legacyNativeScalarCheckpoint, source *LegacyByteSource, extent *int) {
				source.Parts[len(source.Parts)-1].Bytes++
			}},
			{"invalid_hash_descriptor", func(state *legacyNativeScalarCheckpoint, source *LegacyByteSource, extent *int) {
				source.Parts[0].Hash = ContentHash("bad")
			}},
			{"missing_reader", func(state *legacyNativeScalarCheckpoint, source *LegacyByteSource, extent *int) { source.Read = nil }},
		}
		for _, fixture := range fixtures {
			test.Run(fixture.name, func(test *testing.T) {
				state, prefix, storage, sourceBytes := newFixture(LegacyNormalizationPartBytes)
				fixture.mutate(&state, &prefix, &sourceBytes)
				work, err := newLegacyNativeScalarRestore(ctx, state, prefix, sourceBytes, legacyNativeScalarCheckpointProcessBudget)
				if work != nil || !errors.Is(err, ErrIntegrity) || len(storage.reads) != 0 {
					test.Fatalf("malformed admission work=%v reads=%v error=%v", work != nil, storage.reads, err)
				}
			})
		}
		for _, budget := range []int64{-1, 0, 1} {
			state, prefix, storage, sourceBytes := newFixture(0)
			work, err := newLegacyNativeScalarRestore(ctx, state, prefix, sourceBytes, budget)
			want := ErrIntegrity
			if budget > 0 {
				want = ErrDocumentResourceLimit
			}
			if work != nil || !errors.Is(err, want) || len(storage.reads) != 0 {
				test.Fatalf("budget=%d work=%v reads=%v error=%v", budget, work != nil, storage.reads, err)
			}
		}
	})
	test.Run("part_failures_are_sticky", func(test *testing.T) {
		readFailure := errors.New("finite scalar prefix read failure")
		for _, failure := range []string{"short", "oversized", "hash", "io"} {
			test.Run(failure, func(test *testing.T) {
				state, prefix, storage, sourceBytes := newFixture(0)
				switch failure {
				case "short":
					storage.delta = -1
				case "oversized":
					storage.delta = 1
				case "hash":
					prefix.Parts[0].Hash = HashContent([]byte("not the accepted logical prefix"))
				case "io":
					storage.failure = readFailure
				}
				work, err := newLegacyNativeScalarRestore(ctx, state, prefix, sourceBytes, legacyNativeScalarCheckpointProcessBudget)
				if err != nil {
					test.Fatal(err)
				}
				defer work.close()
				var terminal error
				for attempt := 0; attempt < 16; attempt++ {
					ready, stepErr := work.step(ctx)
					if ready {
						test.Fatal("bad prefix published a ready native scalar")
					}
					if stepErr != nil {
						terminal = stepErr
						break
					}
				}
				if !errors.Is(terminal, ErrPreparedDocRangeObject) || failure == "io" && !errors.Is(terminal, readFailure) {
					test.Fatalf("bad prefix error=%v", terminal)
				}
				readCount := len(storage.reads)
				storage.failure, storage.delta = nil, 0
				for attempt := 0; attempt < 2; attempt++ {
					ready, stepErr := work.step(ctx)
					scalar, takeErr := work.take(ctx)
					if ready || scalar != nil || !errors.Is(stepErr, ErrPreparedDocRangeObject) || !errors.Is(takeErr, ErrPreparedDocRangeObject) || len(storage.reads) != readCount {
						test.Fatalf("prefix failure was not sticky: step=%v take=%v reads=%v", stepErr, takeErr, storage.reads)
					}
				}
			})
		}
	})
	test.Run("context_priority_and_sticky_cancel", func(test *testing.T) {
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		state, prefix, storage, sourceBytes := newFixture(0)
		state.Version = 0
		work, err := newLegacyNativeScalarRestore(canceled, state, prefix, sourceBytes, 0)
		if work != nil || !errors.Is(err, context.Canceled) || len(storage.reads) != 0 {
			test.Fatalf("constructor context priority error=%v reads=%v", err, storage.reads)
		}
		for _, stage := range []string{"before_allocation", "before_read", "during_read", "ready_take"} {
			test.Run(stage, func(test *testing.T) {
				state, prefix, storage, sourceBytes := newFixture(0)
				work, err := newLegacyNativeScalarRestore(ctx, state, prefix, sourceBytes, legacyNativeScalarCheckpointProcessBudget)
				if err != nil {
					test.Fatal(err)
				}
				defer work.close()
				active, stop := context.WithCancel(ctx)
				defer stop()
				if stage == "ready_take" {
					legacyNativeScalarCheckpointProcessDrain(test, active, work)
				} else if stage != "before_allocation" {
					ready, err := work.step(active)
					if err != nil || ready || len(storage.reads) != 0 {
						test.Fatalf("separate allocation step ready=%v error=%v", ready, err)
					}
				}
				if stage == "during_read" {
					storage.cancel = stop
				} else {
					stop()
				}
				if stage == "ready_take" {
					scalar, err := work.take(active)
					if scalar != nil || !errors.Is(err, context.Canceled) {
						test.Fatalf("canceled take error=%v", err)
					}
				} else {
					ready, err := work.step(active)
					if ready || !errors.Is(err, context.Canceled) {
						test.Fatalf("canceled step ready=%v error=%v", ready, err)
					}
				}
				readCount := len(storage.reads)
				ready, stepErr := work.step(ctx)
				scalar, takeErr := work.take(ctx)
				if ready || scalar != nil || !errors.Is(stepErr, context.Canceled) || !errors.Is(takeErr, context.Canceled) || len(storage.reads) != readCount {
					test.Fatalf("cancel did not remain sticky: step=%v take=%v", stepErr, takeErr)
				}
				for _, actual := range storage.contexts {
					if actual != active {
						test.Fatal("prefix IO used a stale or background context")
					}
				}
			})
		}
	})
	test.Run("descriptor_borrowed_page_and_take_ownership", func(test *testing.T) {
		state, prefix, storage, sourceBytes := newFixture(0)
		work, err := newLegacyNativeScalarRestore(ctx, state, prefix, sourceBytes, legacyNativeScalarCheckpointProcessBudget)
		if err != nil {
			test.Fatal(err)
		}
		defer work.close()
		for ordinal := range prefix.Parts {
			prefix.Parts[ordinal] = ConversationManifestChunk{Hash: HashContent([]byte("caller mutation")), Bytes: 1}
		}
		prefix.Bytes, prefix.PartBytes, prefix.Read = 0, 1, nil
		ready := false
		for attempt := 0; attempt < 64 && !ready; attempt++ {
			ready, err = work.step(ctx)
			if err != nil {
				test.Fatal(err)
			}
			clear(storage.borrowed)
		}
		if !ready {
			test.Fatal("aliased restore exceeded finite step bound")
		}
		scalar, err := work.take(ctx)
		if err != nil || scalar == nil || scalar.text == nil || scalar.text.String() != string(payload) {
			test.Fatalf("caller metadata/page mutation corrupted restore: %v", err)
		}
		if repeated, err := work.take(ctx); repeated != nil || !errors.Is(err, ErrIntegrity) {
			test.Fatalf("take transferred native scalar twice: %v", err)
		}
		work.close()
		work.close()
		if scalar.text.String() != string(payload) || scalar.position != state.Position || scalar.end != state.End {
			test.Fatal("closing restore altered the transferred native scalar")
		}
	})
	test.Run("close_before_read", func(test *testing.T) {
		state, prefix, storage, sourceBytes := newFixture(0)
		work, err := newLegacyNativeScalarRestore(ctx, state, prefix, sourceBytes, legacyNativeScalarCheckpointProcessBudget)
		if err != nil {
			test.Fatal(err)
		}
		work.close()
		ready, stepErr := work.step(ctx)
		scalar, takeErr := work.take(ctx)
		if ready || scalar != nil || !errors.Is(stepErr, ErrIntegrity) || !errors.Is(takeErr, ErrIntegrity) || len(storage.reads) != 0 {
			test.Fatalf("closed restore step=%v take=%v reads=%v", stepErr, takeErr, storage.reads)
		}
	})
	test.Run("snapshot_copy_bounds_close_and_empty_presence", func(test *testing.T) {
		builder := &strings.Builder{}
		builder.Grow(len(payload) + 128)
		_, _ = builder.Write(payload)
		original := &legacyNativeScalar{text: builder, position: LegacyNormalizationPartBytes, end: LegacyNormalizationPartBytes + 31, maximum: 3 * (LegacyNormalizationPartBytes + 31)}
		snapshot, err := original.checkpoint(ctx)
		if err != nil {
			test.Fatal(err)
		}
		retained, err := snapshot.retainedBytes()
		if err != nil || retained < int64(len(payload)) {
			test.Fatalf("snapshot retained bytes=%d error=%v", retained, err)
		}
		part, err := snapshot.read(ctx, 0, LegacyCanonicalEncodingBytes)
		if err != nil || !bytes.Equal(part, payload[:LegacyCanonicalEncodingBytes]) {
			test.Fatalf("snapshot bounded read error=%v", err)
		}
		clear(part)
		if repeated, err := snapshot.read(ctx, 0, len(part)); err != nil || !bytes.Equal(repeated, payload[:len(part)]) {
			test.Fatalf("snapshot returned mutable prefix alias: %v", err)
		}
		for _, request := range [][2]int{{-1, 1}, {0, -1}, {0, LegacyCanonicalEncodingBytes + 1}, {len(payload), 1}, {len(payload) + 1, 0}} {
			if _, err := snapshot.read(ctx, request[0], request[1]); !errors.Is(err, ErrIntegrity) {
				test.Fatalf("snapshot read=%v error=%v", request, err)
			}
		}
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		if _, err := snapshot.read(canceled, -1, -1); !errors.Is(err, context.Canceled) {
			test.Fatalf("snapshot read context priority error=%v", err)
		}
		if _, err := original.checkpoint(canceled); !errors.Is(err, context.Canceled) {
			test.Fatalf("snapshot capture context priority error=%v", err)
		}
		snapshot.close()
		if snapshot.prefix != "" || original.text.String() != string(payload) {
			test.Fatal("snapshot close retained its text or changed the original builder")
		}
		if _, err := snapshot.read(ctx, 0, 1); !errors.Is(err, ErrIntegrity) {
			test.Fatalf("closed snapshot read error=%v", err)
		}
		for _, present := range []bool{false, true} {
			scalar := &legacyNativeScalar{maximum: 128}
			if present {
				scalar.text = &strings.Builder{}
			}
			empty, err := scalar.checkpoint(ctx)
			if err != nil {
				test.Fatal(err)
			}
			if empty.state.Present != present || empty.state.Bytes != 0 {
				test.Fatal("snapshot conflated absent and present-empty text")
			}
			prefix, _ := legacyNativeScalarCheckpointProcessSource(nil, 0)
			work, err := newLegacyNativeScalarRestore(ctx, empty.state, prefix, 0, legacyNativeScalarCheckpointProcessBudget)
			empty.close()
			if err != nil {
				test.Fatal(err)
			}
			legacyNativeScalarCheckpointProcessDrain(test, ctx, work)
			restored, err := work.take(ctx)
			work.close()
			if err != nil || restored == nil || (restored.text != nil) != present || restored.position != 0 || restored.end != 0 {
				test.Fatalf("empty presence=%v error=%v", present, err)
			}
		}
	})
}
