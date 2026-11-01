package domain

import (
	"bytes"
	"context"
	"encoding/json"
)

const (
	docMessageInputFirst = iota + 1
	docMessageInputValue
	docMessageInputObject
	docMessageInputAfter
	docMessageInputComplete
)

type DocMessageInputState struct {
	Version   int                          `json:"version"`
	Source    ContentHash                  `json:"source"`
	Next      int                          `json:"next"`
	Phase     int                          `json:"phase"`
	Count     int                          `json:"count"`
	LastTo    int                          `json:"last_to"`
	BlockFrom int                          `json:"block_from"`
	Syntax    *DocFieldDiscoveryState      `json:"syntax,omitempty"`
	Entries   []DocMessageBlockInput       `json:"entries,omitempty"`
	Directory DocRangeDirectoryAccumulator `json:"directory"`
}

func NewDocMessageInputState(source DocMessageInputSource) (DocMessageInputState, error) {
	hash, err := source.Hash()
	if err != nil {
		return DocMessageInputState{}, err
	}
	if source.Empty {
		return DocMessageInputState{Version: 1, Source: hash, Phase: docMessageInputComplete}, nil
	}
	return DocMessageInputState{Version: 1, Source: hash, Next: source.Blocks.From + 1, Phase: docMessageInputFirst, LastTo: source.Blocks.From + 1}, nil
}

func (state DocMessageInputState) Done() bool { return state.Phase == docMessageInputComplete }

func (state DocMessageInputState) Validate(source DocMessageInputSource) error {
	hash, err := source.Hash()
	if err == nil && source.Empty {
		initial, err := NewDocMessageInputState(source)
		if err != nil {
			return err
		}
		actual, _ := canonicalJSON(state)
		expected, _ := canonicalJSON(initial)
		if !bytes.Equal(actual, expected) {
			return ErrIntegrity
		}
		return nil
	}
	if err != nil || state.Version != 1 || state.Source != hash || state.Next <= source.Blocks.From || state.Next > source.Blocks.To || state.Phase < docMessageInputFirst || state.Phase > docMessageInputComplete || state.Count < 0 || state.Count > source.maximumEntries() || state.LastTo <= source.Blocks.From || state.LastTo > state.Next || state.LastTo >= source.Blocks.To || len(state.Entries) >= DocMessageInputPageEntries || state.Directory.Validate() != nil || state.Done() != (state.Next == source.Blocks.To) {
		return ErrIntegrity
	}
	if state.Count == 0 && (state.LastTo != source.Blocks.From+1 || state.Directory.Count != 0 || len(state.Entries) != 0) || state.Count > 0 && int64(state.LastTo-source.Blocks.From) < int64(state.Count)*3 {
		return ErrIntegrity
	}
	if state.Done() {
		if len(state.Entries) != 0 || state.Directory.Count != (state.Count+DocMessageInputPageEntries-1)/DocMessageInputPageEntries || state.Syntax != nil || state.BlockFrom != 0 {
			return ErrIntegrity
		}
	} else if state.Count != state.Directory.Count*DocMessageInputPageEntries+len(state.Entries) {
		return ErrIntegrity
	}
	previous := source.Blocks.From
	for index, entry := range state.Entries {
		if entry.Validate(source) != nil || entry.Index != state.Directory.Count*DocMessageInputPageEntries+index || entry.Anchor.From <= previous || entry.Anchor.To > state.Next {
			return ErrIntegrity
		}
		previous = entry.Anchor.To
	}
	if len(state.Entries) > 0 && state.Entries[len(state.Entries)-1].Anchor.To != state.LastTo {
		return ErrIntegrity
	}
	if state.Phase == docMessageInputFirst && state.Count != 0 || (state.Phase == docMessageInputValue || state.Phase == docMessageInputAfter) && state.Count == 0 {
		return ErrIntegrity
	}
	if state.Phase == docMessageInputObject {
		if state.Syntax == nil || state.BlockFrom <= source.Blocks.From || state.BlockFrom < state.LastTo || state.Count > 0 && state.BlockFrom == state.LastTo || state.BlockFrom >= state.Next || state.Syntax.Version != 1 || state.Syntax.EventHash != source.EventHash || state.Syntax.EventBytes != source.EventBytes || state.Syntax.Next != state.Next || !state.Syntax.Started || state.Syntax.RootDone || state.Syntax.Done || len(state.Syntax.HashState) != 0 || state.Syntax.Digest != "" || state.Syntax.validateSyntax(docFieldSchemaBlock) != nil {
			return ErrIntegrity
		}
		for _, span := range []*DocFindSpan{state.Syntax.Blocks, state.Syntax.Output, state.Syntax.Summary} {
			if span != nil && span.From <= state.BlockFrom {
				return ErrIntegrity
			}
		}
	} else if state.Syntax != nil || state.BlockFrom != 0 {
		return ErrIntegrity
	}
	return nil
}

func (state DocMessageInputState) Encode(source DocMessageInputSource) ([]byte, error) {
	if state.Validate(source) != nil {
		return nil, ErrIntegrity
	}
	raw, err := canonicalJSON(state)
	if err != nil || len(raw) > MaxDocDisplayStateBytes {
		return nil, ErrIntegrity
	}
	return raw, nil
}

func DecodeDocMessageInputState(source DocMessageInputSource, expected ContentHash, raw []byte) (DocMessageInputState, error) {
	var state DocMessageInputState
	if len(raw) == 0 || len(raw) > MaxDocDisplayStateBytes || ValidateContentHash(expected) != nil || HashContent(raw) != expected || json.Unmarshal(raw, &state) != nil {
		return DocMessageInputState{}, ErrIntegrity
	}
	if state.Syntax != nil {
		state.Syntax.schema = docFieldSchemaBlock
	}
	canonical, err := state.Encode(source)
	if err != nil || !bytes.Equal(raw, canonical) {
		return DocMessageInputState{}, ErrIntegrity
	}
	return state, nil
}

type DocMessageInputStep struct {
	State     DocMessageInputState
	Objects   []DocRangeObject
	ReadBytes int
}

func StepDocMessageInput(ctx context.Context, source DocMessageInputSource, prior DocMessageInputState, loadRaw func(context.Context, int, int) ([]byte, error), loadObject func(context.Context, ContentHash) ([]byte, error)) (result DocMessageInputStep, failure error) {
	defer func() {
		if err := ctx.Err(); err != nil {
			result, failure = DocMessageInputStep{}, err
		}
	}()
	if err := ctx.Err(); err != nil {
		return DocMessageInputStep{}, err
	}
	if prior.Validate(source) != nil || prior.Done() || loadRaw == nil {
		return DocMessageInputStep{}, ErrIntegrity
	}
	if err := ValidateDocMessageInputCheckpoint(ctx, source, prior, loadObject); err != nil {
		return DocMessageInputStep{}, err
	}
	encoded, err := prior.Encode(source)
	if err != nil {
		return DocMessageInputStep{}, err
	}
	next, err := DecodeDocMessageInputState(source, HashContent(encoded), encoded)
	if err != nil {
		return DocMessageInputStep{}, err
	}
	startCount := next.Count
	for !next.Done() && result.ReadBytes < DocDisplayStepSourceBytes && next.Count-startCount < DocMessageInputStepEntries {
		length := min(4096, source.Blocks.To-next.Next, DocDisplayStepSourceBytes-result.ReadBytes)
		raw, err := loadRaw(ctx, next.Next, length)
		if err != nil {
			return DocMessageInputStep{}, err
		}
		if len(raw) != length {
			return DocMessageInputStep{}, ErrIntegrity
		}
		result.ReadBytes += length
		for _, character := range raw {
			if err := ctx.Err(); err != nil {
				return DocMessageInputStep{}, err
			}
			if next.Done() || next.Count-startCount == DocMessageInputStepEntries {
				break
			}
			if next.Phase == docMessageInputObject {
				for {
					consumed, err := next.Syntax.accept(character)
					if err != nil {
						return DocMessageInputStep{}, err
					}
					if consumed {
						next.Syntax.Next++
						next.Next++
						break
					}
				}
				if next.Syntax.RootDone {
					if err := finishDocMessageInputBlock(ctx, source, &next, &result.Objects); err != nil {
						return DocMessageInputStep{}, err
					}
				}
				continue
			}
			if docFieldSpace(character) {
				next.Next++
				continue
			}
			switch next.Phase {
			case docMessageInputFirst, docMessageInputValue:
				if character == ']' && next.Phase == docMessageInputFirst {
					next.Next++
					if err := completeDocMessageInput(ctx, source, &next, &result.Objects); err != nil {
						return DocMessageInputStep{}, err
					}
					continue
				}
				if character != '{' {
					return DocMessageInputStep{}, ErrIntegrity
				}
				parser := newDocBlockFieldParser(source.EventHash, source.EventBytes, next.Next)
				consumed, err := parser.accept(character)
				if err != nil || !consumed {
					return DocMessageInputStep{}, ErrIntegrity
				}
				next.BlockFrom, next.Syntax, next.Phase = next.Next, &parser, docMessageInputObject
				next.Syntax.Next++
				next.Next++
			case docMessageInputAfter:
				next.Next++
				switch character {
				case ',':
					next.Phase = docMessageInputValue
				case ']':
					if err := completeDocMessageInput(ctx, source, &next, &result.Objects); err != nil {
						return DocMessageInputStep{}, err
					}
				default:
					return DocMessageInputStep{}, ErrIntegrity
				}
			default:
				return DocMessageInputStep{}, ErrIntegrity
			}
		}
	}
	if next.Next <= prior.Next || next.Validate(source) != nil {
		return DocMessageInputStep{}, ErrIntegrity
	}
	result.State = next
	return result, nil
}

func finishDocMessageInputBlock(ctx context.Context, source DocMessageInputSource, state *DocMessageInputState, objects *[]DocRangeObject) error {
	parser := state.Syntax
	if parser.InvalidBlocks || parser.InvalidSummary || !source.Summary && parser.Kind != EventKind("text") && parser.OutputNonSpace {
		return ErrIntegrity
	}
	entry := DocMessageBlockInput{Index: state.Count, Anchor: DocFindSpan{From: state.BlockFrom, To: state.Next}, Type: parser.Blocks, Name: parser.Output, Text: parser.Summary, IsText: parser.Kind == EventKind("text")}
	if entry.Validate(source) != nil {
		return ErrIntegrity
	}
	state.Count++
	state.LastTo = state.Next
	state.Entries = append(state.Entries, entry)
	state.Syntax, state.BlockFrom, state.Phase = nil, 0, docMessageInputAfter
	if len(state.Entries) == DocMessageInputPageEntries {
		return flushDocMessageInputPage(ctx, source, state, objects)
	}
	return nil
}

func flushDocMessageInputPage(ctx context.Context, source DocMessageInputSource, state *DocMessageInputState, objects *[]DocRangeObject) error {
	if len(state.Entries) == 0 {
		return nil
	}
	page := DocMessageInputPage{Version: 1, Source: state.Source, Index: state.Directory.Count, Entries: state.Entries}
	raw, err := page.Encode(source)
	if err != nil {
		return err
	}
	directory, produced, err := state.Directory.Append(ctx, HashContent(raw))
	if err != nil {
		return err
	}
	*objects = append(*objects, produced...)
	*objects = append(*objects, DocRangeObject{Hash: HashContent(raw), Bytes: raw})
	state.Directory, state.Entries = directory, nil
	return nil
}

func completeDocMessageInput(ctx context.Context, source DocMessageInputSource, state *DocMessageInputState, objects *[]DocRangeObject) error {
	if state.Next != source.Blocks.To {
		return ErrIntegrity
	}
	if err := flushDocMessageInputPage(ctx, source, state, objects); err != nil {
		return err
	}
	state.Phase = docMessageInputComplete
	return nil
}

func ValidateDocMessageInputCheckpoint(ctx context.Context, source DocMessageInputSource, state DocMessageInputState, load func(context.Context, ContentHash) ([]byte, error)) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if state.Validate(source) != nil {
		return ErrIntegrity
	}
	if state.Directory.Count == 0 {
		return nil
	}
	if load == nil {
		return ErrIntegrity
	}
	root, staged, err := state.Directory.Finish(ctx)
	if err != nil {
		return err
	}
	selected := func(ctx context.Context, hash ContentHash) ([]byte, error) {
		for _, object := range staged {
			if object.Hash == hash {
				return object.Bytes, nil
			}
		}
		return load(ctx, hash)
	}
	hash, err := ResolveDocRangeLeaf(ctx, root, state.Directory.Count-1, selected)
	if err != nil {
		return err
	}
	raw, err := selected(ctx, hash)
	if err != nil {
		return err
	}
	page, err := DecodeDocMessageInputPage(source, hash, raw)
	if err != nil || page.Index != state.Directory.Count-1 {
		return ErrIntegrity
	}
	last := page.Entries[len(page.Entries)-1]
	if len(state.Entries) > 0 {
		if len(page.Entries) != DocMessageInputPageEntries || last.Index+1 != state.Entries[0].Index || last.Anchor.To >= state.Entries[0].Anchor.From {
			return ErrIntegrity
		}
	} else if last.Index+1 != state.Count || last.Anchor.To != state.LastTo {
		return ErrIntegrity
	}
	return ctx.Err()
}

func FinishDocMessageInput(ctx context.Context, source DocMessageInputSource, state DocMessageInputState) (result DocMessageInputGeneration, objects []DocRangeObject, failure error) {
	defer func() {
		if err := ctx.Err(); err != nil {
			result, objects, failure = DocMessageInputGeneration{}, nil, err
		}
	}()
	if err := ctx.Err(); err != nil {
		return DocMessageInputGeneration{}, nil, err
	}
	if state.Validate(source) != nil || !state.Done() {
		return DocMessageInputGeneration{}, nil, ErrIntegrity
	}
	root, objects, err := state.Directory.Finish(ctx)
	if err != nil {
		return DocMessageInputGeneration{}, nil, err
	}
	generation := DocMessageInputGeneration{Version: 1, Source: source, Count: state.Count, Pages: root}
	if generation.Validate() != nil {
		return DocMessageInputGeneration{}, nil, ErrIntegrity
	}
	return generation, objects, nil
}
