package domain

import (
	"context"
)

const DocVisibleAlgorithm = 1
const DocVisiblePageUnits = 128
const MaxDocVisibleObjectBytes = 32 << 10
const MaxDocVisibleGenerationBytes = 2048

type DocVisibleUnit struct {
	From           int `json:"from"`
	To             int `json:"to"`
	EventIndex     int `json:"event_index"`
	TurnIndex      int `json:"turn_index"`
	RenderedBefore int `json:"rendered_before"`
	FoldRows       int `json:"fold_rows"`
}

func (unit DocVisibleUnit) RenderedEnd() int {
	end := unit.RenderedBefore + unit.FoldRows
	if unit.EventIndex >= 0 {
		end++
	}
	return end
}

func (unit DocVisibleUnit) Validate(total int, mode string) error {
	if total < 0 || total > MaxConversationDocumentBytes || unit.From < 0 || unit.To <= unit.From || unit.To > total ||
		unit.TurnIndex < -1 || unit.TurnIndex > unit.From || unit.RenderedBefore < 0 || unit.RenderedBefore > unit.From ||
		unit.FoldRows < 0 || unit.FoldRows > unit.To-unit.From || unit.RenderedEnd() > unit.To ||
		unit.EventIndex < -1 || unit.EventIndex >= 0 && (unit.EventIndex < unit.From || unit.EventIndex >= unit.To) {
		return ErrIntegrity
	}
	if unit.EventIndex >= 0 && unit.FoldRows >= unit.To-unit.From || unit.TurnIndex == unit.From && unit.EventIndex != unit.From {
		return ErrIntegrity
	}
	switch mode {
	case "all":
		if unit.To != unit.From+1 || unit.FoldRows != 0 {
			return ErrIntegrity
		}
	case "chat":
		if unit.EventIndex >= 0 && unit.EventIndex != unit.To-1 {
			return ErrIntegrity
		}
	case "prompts":
		if unit.EventIndex >= 0 && (unit.EventIndex != unit.From || unit.TurnIndex != unit.From) {
			return ErrIntegrity
		}
	default:
		return ErrValidation
	}
	return nil
}

type DocVisibleRoot struct {
	Hash  ContentHash `json:"hash"`
	Units int         `json:"units"`
}

type DocVisibleGeneration struct {
	Version         int                       `json:"version"`
	Algorithm       int                       `json:"algorithm"`
	Hash            ContentHash               `json:"hash"`
	RangeGeneration ContentHash               `json:"range_generation"`
	Events          int                       `json:"events"`
	Rendered        int                       `json:"rendered"`
	Views           map[string]DocVisibleRoot `json:"views"`
}

func (generation DocVisibleGeneration) Validate(hash, ranges ContentHash) error {
	if generation.Version != 1 || generation.Algorithm != DocVisibleAlgorithm || generation.Hash != hash || generation.RangeGeneration != ranges ||
		ValidateContentHash(hash) != nil || ValidateContentHash(ranges) != nil || generation.Events < 0 || generation.Events > MaxConversationDocumentBytes ||
		generation.Rendered < 0 || generation.Rendered > generation.Events || len(generation.Views) != 3 {
		return ErrIntegrity
	}
	for _, mode := range []string{"all", "chat", "prompts"} {
		root, exists := generation.Views[mode]
		if !exists || root.Units < 0 || root.Units > generation.Events || (root.Units == 0) != (generation.Events == 0) ||
			root.Units == 0 && root.Hash != "" || root.Units > 0 && ValidateContentHash(root.Hash) != nil || mode == "all" && root.Units != generation.Events {
			return ErrIntegrity
		}
	}
	return nil
}

func (generation DocVisibleGeneration) Encode() ([]byte, error) {
	if err := generation.Validate(generation.Hash, generation.RangeGeneration); err != nil {
		return nil, err
	}
	raw, err := canonicalJSON(generation)
	if err != nil || len(raw) > MaxDocVisibleGenerationBytes {
		return nil, ErrIntegrity
	}
	return raw, nil
}

type docVisibleSpan struct {
	Hash     ContentHash `json:"hash"`
	First    int         `json:"first"`
	Limit    int         `json:"limit"`
	From     int         `json:"from"`
	To       int         `json:"to"`
	Rank     int         `json:"rank"`
	Rendered int         `json:"rendered"`
}

type docVisibleDirectory struct {
	Level    int              `json:"level"`
	Children []docVisibleSpan `json:"children"`
}

type docVisiblePage struct {
	Start int              `json:"start"`
	Units []DocVisibleUnit `json:"units"`
}

type docVisibleBuilder struct {
	ctx     context.Context
	mode    string
	total   int
	count   int
	units   []DocVisibleUnit
	spans   []docVisibleSpan
	pending *DocVisibleUnit
	emit    func(context.Context, DocRangeObject) error
}

func (builder *docVisibleBuilder) object(value any) (ContentHash, error) {
	if err := builder.ctx.Err(); err != nil {
		return "", err
	}
	raw, err := canonicalDocVisibleObjectJSON(value)
	if err != nil || len(raw) > MaxDocVisibleObjectBytes {
		return "", ErrIntegrity
	}
	hash := HashContent(raw)
	if err := builder.emit(builder.ctx, DocRangeObject{Hash: hash, Bytes: raw}); err != nil {
		return "", err
	}
	return hash, builder.ctx.Err()
}

func (builder *docVisibleBuilder) flush() error {
	if len(builder.units) == 0 {
		return nil
	}
	start := builder.count - len(builder.units)
	hash, err := builder.object(docVisiblePage{Start: start, Units: builder.units})
	if err != nil {
		return err
	}
	first, last := builder.units[0], builder.units[len(builder.units)-1]
	builder.spans = append(builder.spans, docVisibleSpan{Hash: hash, First: start, Limit: builder.count, From: first.From, To: last.To, Rank: first.RenderedBefore, Rendered: last.RenderedEnd()})
	builder.units = builder.units[:0]
	return nil
}

func (builder *docVisibleBuilder) append(unit DocVisibleUnit) error {
	if err := unit.Validate(builder.total, builder.mode); err != nil {
		return err
	}
	builder.units = append(builder.units, unit)
	builder.count++
	if len(builder.units) == DocVisiblePageUnits {
		return builder.flush()
	}
	return nil
}

func (builder *docVisibleBuilder) finishPending() error {
	if builder.pending == nil {
		return nil
	}
	unit := *builder.pending
	builder.pending = nil
	return builder.append(unit)
}

func (builder *docVisibleBuilder) add(entry DocRangeEntry, turn int, prompt bool) error {
	unit := DocVisibleUnit{From: entry.Index, To: entry.Index + 1, EventIndex: entry.Index, TurnIndex: turn, RenderedBefore: entry.RenderedBefore}
	if builder.mode == "all" {
		if entry.EmptyReasoning {
			unit.EventIndex = -1
		}
		return builder.append(unit)
	}
	if prompt {
		if err := builder.finishPending(); err != nil {
			return err
		}
		if builder.mode == "chat" {
			return builder.append(unit)
		}
		builder.pending = &unit
		return nil
	}
	if builder.pending == nil {
		unit.EventIndex = -1
		builder.pending = &unit
	}
	builder.pending.To = entry.Index + 1
	if builder.mode == "chat" && entry.Kind == EventMessage {
		builder.pending.EventIndex = entry.Index
		return builder.finishPending()
	}
	if !entry.EmptyReasoning {
		builder.pending.FoldRows++
	}
	return nil
}

func (builder *docVisibleBuilder) finish() (DocVisibleRoot, error) {
	if err := builder.finishPending(); err != nil {
		return DocVisibleRoot{}, err
	}
	if err := builder.flush(); err != nil {
		return DocVisibleRoot{}, err
	}
	root := DocVisibleRoot{Units: builder.count}
	for level := 0; len(builder.spans) > 0; level++ {
		parents := make([]docVisibleSpan, 0, (len(builder.spans)+DocRangeDirectoryFanout-1)/DocRangeDirectoryFanout)
		for start := 0; start < len(builder.spans); start += DocRangeDirectoryFanout {
			children := builder.spans[start:min(start+DocRangeDirectoryFanout, len(builder.spans))]
			hash, err := builder.object(docVisibleDirectory{Level: level, Children: children})
			if err != nil {
				return DocVisibleRoot{}, err
			}
			first, last := children[0], children[len(children)-1]
			parents = append(parents, docVisibleSpan{Hash: hash, First: first.First, Limit: last.Limit, From: first.From, To: last.To, Rank: first.Rank, Rendered: last.Rendered})
		}
		if len(parents) == 1 {
			root.Hash = parents[0].Hash
			return root, nil
		}
		builder.spans = parents
	}
	return root, builder.ctx.Err()
}

func BuildDocVisibleGeneration(ctx context.Context, proof VerifiedDocRangeGeneration, emit func(context.Context, DocRangeObject) error) (DocVisibleGeneration, error) {
	return buildDocVisibleGeneration(ctx, proof, emit, nil)
}

func BuildDocVisibleGenerationWithRangePages(ctx context.Context, proof VerifiedDocRangeGeneration, emit func(context.Context, DocRangeObject) error, emitRangePage func(context.Context, int, DocRangeObject) error) (DocVisibleGeneration, error) {
	if err := ctx.Err(); err != nil {
		return DocVisibleGeneration{}, err
	}
	if emitRangePage == nil {
		return DocVisibleGeneration{}, ErrIntegrity
	}
	return buildDocVisibleGeneration(ctx, proof, emit, emitRangePage)
}

func buildDocVisibleGeneration(ctx context.Context, proof VerifiedDocRangeGeneration, emit func(context.Context, DocRangeObject) error, emitRangePage func(context.Context, int, DocRangeObject) error) (DocVisibleGeneration, error) {
	if err := ctx.Err(); err != nil {
		return DocVisibleGeneration{}, err
	}
	verified, ranges := proof.index, proof.hash
	if emit == nil || ValidateContentHash(ranges) != nil || verified.read == nil || verified.presentationAlgorithm != DocVisibleAlgorithm || verified.index.Validate(verified.index.Hash) != nil {
		return DocVisibleGeneration{}, ErrIntegrity
	}
	generation := DocVisibleGeneration{Version: 1, Algorithm: DocVisibleAlgorithm, Hash: verified.index.Hash, RangeGeneration: ranges, Events: int(verified.index.Manifest.EventCount), Views: make(map[string]DocVisibleRoot, 3)}
	builders := make([]docVisibleBuilder, 0, 3)
	for _, mode := range []string{"all", "chat", "prompts"} {
		builders = append(builders, docVisibleBuilder{ctx: ctx, mode: mode, total: generation.Events, emit: emit})
	}
	turn, next := -1, 0
	for page := range verified.index.Pages {
		raw, entries, err := verified.pageEntries(ctx, page)
		if err != nil {
			return DocVisibleGeneration{}, err
		}
		for _, entry := range entries {
			if err := ctx.Err(); err != nil {
				return DocVisibleGeneration{}, err
			}
			if entry.Index != next || entry.RenderedBefore != generation.Rendered {
				return DocVisibleGeneration{}, ErrIntegrity
			}
			prompt := entry.Kind == EventMessage && entry.Role == string(RoleUser) && !entry.CompactSummary && !entry.SeedSummary
			if prompt {
				turn = entry.Index
			}
			for position := range builders {
				if err := builders[position].add(entry, turn, prompt); err != nil {
					return DocVisibleGeneration{}, err
				}
			}
			if !entry.EmptyReasoning {
				generation.Rendered++
			}
			next++
		}
		if emitRangePage != nil {
			if err := ctx.Err(); err != nil {
				return DocVisibleGeneration{}, err
			}
			if err := emitRangePage(ctx, page, DocRangeObject{Hash: verified.index.Pages[page], Bytes: raw}); err != nil {
				return DocVisibleGeneration{}, err
			}
		}
	}
	if next != generation.Events {
		return DocVisibleGeneration{}, ErrIntegrity
	}
	for position := range builders {
		root, err := builders[position].finish()
		if err != nil {
			return DocVisibleGeneration{}, err
		}
		generation.Views[builders[position].mode] = root
	}
	if err := generation.Validate(generation.Hash, ranges); err != nil {
		return DocVisibleGeneration{}, err
	}
	if err := ctx.Err(); err != nil {
		return DocVisibleGeneration{}, err
	}
	return generation, nil
}
