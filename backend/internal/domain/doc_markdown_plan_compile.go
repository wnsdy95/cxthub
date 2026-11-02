package domain

import (
	"context"
	"crypto/sha256"
	"encoding"
	"encoding/hex"
	"errors"
	"hash"
	"unicode/utf8"
)

var errDocMarkdownPlanYield = errors.New("Markdown plan step must yield")

type DocMarkdownPlanStep struct {
	State     DocMarkdownPlanState
	Objects   []DocRangeObject
	ReadBytes int
}

type docMarkdownInputCache struct {
	from int
	raw  []byte
}

type docMarkdownPlanCompiler struct {
	ctx        context.Context
	state      DocMarkdownPlanState
	load       func(context.Context, int, int) ([]byte, error)
	caches     [6]docMarkdownInputCache
	readBytes  int
	operations int
	objects    []DocRangeObject
	digest     hash.Hash
}

func StepDocMarkdownPlan(ctx context.Context, prior DocMarkdownPlanState, load func(context.Context, int, int) ([]byte, error)) (result DocMarkdownPlanStep, failure error) {
	defer func() {
		if err := ctx.Err(); err != nil {
			result, failure = DocMarkdownPlanStep{}, err
		}
	}()
	if err := ctx.Err(); err != nil {
		return DocMarkdownPlanStep{}, err
	}
	if prior.Validate() != nil || prior.Phase == docMarkdownPlanDone || load == nil {
		return DocMarkdownPlanStep{}, ErrIntegrity
	}
	encoded, err := prior.Encode()
	if err != nil {
		return DocMarkdownPlanStep{}, err
	}
	state, err := DecodeDocMarkdownPlanState(HashContent(encoded), encoded)
	if err != nil {
		return DocMarkdownPlanStep{}, err
	}
	compiler := docMarkdownPlanCompiler{ctx: ctx, state: state, load: load}
	if state.Digest == "" {
		compiler.digest = sha256.New()
		if err := compiler.digest.(encoding.BinaryUnmarshaler).UnmarshalBinary(state.HashState); err != nil {
			return DocMarkdownPlanStep{}, ErrIntegrity
		}
	}
	for actions := 0; actions < DocMarkdownPlanStepActions && compiler.state.Phase != docMarkdownPlanDone; actions++ {
		if err := ctx.Err(); err != nil {
			return DocMarkdownPlanStep{}, err
		}
		if compiler.operations > DocMarkdownPlanStepOperations-4 {
			break
		}
		if err := compiler.advance(); err != nil {
			if errors.Is(err, errDocMarkdownPlanYield) {
				break
			}
			return DocMarkdownPlanStep{}, err
		}
	}
	if compiler.state.Digest == "" {
		compiler.state.HashState, err = compiler.digest.(encoding.BinaryMarshaler).MarshalBinary()
		if err != nil {
			return DocMarkdownPlanStep{}, err
		}
	}
	if compiler.state.Validate() != nil {
		return DocMarkdownPlanStep{}, ErrIntegrity
	}
	return DocMarkdownPlanStep{State: compiler.state, Objects: compiler.objects, ReadBytes: compiler.readBytes}, nil
}

func (compiler *docMarkdownPlanCompiler) read(from, length, slot int) ([]byte, error) {
	if err := compiler.ctx.Err(); err != nil {
		return nil, err
	}
	if from < 0 || length <= 0 || length > compiler.state.Input.Bytes || from > compiler.state.Input.Bytes-length {
		return nil, ErrIntegrity
	}
	for _, cache := range compiler.caches {
		if from >= cache.from && from-cache.from <= len(cache.raw)-length {
			return cache.raw[from-cache.from : from-cache.from+length], nil
		}
	}
	count := min(4096, compiler.state.Input.Bytes-from, DocDisplayStepSourceBytes-compiler.readBytes)
	if count < length {
		return nil, errDocMarkdownPlanYield
	}
	raw, err := compiler.load(compiler.ctx, from, count)
	if err != nil {
		return nil, err
	}
	if len(raw) != count {
		return nil, ErrIntegrity
	}
	compiler.readBytes += count
	cache := &compiler.caches[slot]
	cache.from, cache.raw = from, append(cache.raw[:0], raw...)
	return cache.raw[:length], nil
}

func (compiler *docMarkdownPlanCompiler) character(from, slot int) (rune, int, error) {
	raw, err := compiler.read(from, min(utf8.UTFMax, compiler.state.Input.Bytes-from), slot)
	if err != nil {
		return 0, 0, err
	}
	character, width := utf8.DecodeRune(raw)
	if character == utf8.RuneError && width == 1 {
		return 0, 0, ErrIntegrity
	}
	return character, width, nil
}

func (compiler *docMarkdownPlanCompiler) syncHash(final bool) error {
	state := &compiler.state
	if state.Digest != "" {
		return nil
	}
	for state.Scanned-state.HashNext >= sha256.BlockSize {
		raw, err := compiler.read(state.HashNext, sha256.BlockSize, 5)
		if err != nil {
			return err
		}
		_, _ = compiler.digest.Write(raw)
		state.HashNext += len(raw)
	}
	if !final {
		return nil
	}
	if state.HashNext < state.Input.Bytes {
		raw, err := compiler.read(state.HashNext, state.Input.Bytes-state.HashNext, 5)
		if err != nil {
			return err
		}
		_, _ = compiler.digest.Write(raw)
	}
	actual := ContentHash("sha256:" + hex.EncodeToString(compiler.digest.Sum(nil)))
	if actual != state.Input.Hash {
		return ErrIntegrity
	}
	state.Digest, state.HashNext, state.HashState = actual, state.Input.Bytes, nil
	return nil
}

func (compiler *docMarkdownPlanCompiler) line() (*docMarkdownPlanLine, error) {
	state := &compiler.state
	if state.PendingLine != nil {
		line := state.PendingLine
		state.PendingLine = nil
		return line, nil
	}
	if state.LineDone {
		return nil, nil
	}
	for {
		if err := compiler.syncHash(state.Line.Next == state.Input.Bytes); err != nil {
			return nil, err
		}
		if state.Line.Next == state.Input.Bytes {
			line, err := state.Line.finish()
			state.LineDone = true
			return &line, err
		}
		character, width, err := compiler.character(state.Line.Next, 0)
		if err != nil {
			return nil, err
		}
		if character == '\n' {
			line, err := state.Line.finish()
			state.Scanned = line.Next
			state.Line = newDocMarkdownLineScan(line.Next)
			return &line, err
		}
		if err := state.Line.consume(character, width); err != nil {
			return nil, err
		}
		state.Scanned = state.Line.Next
	}
}

func (compiler *docMarkdownPlanCompiler) advance() error {
	state := &compiler.state
	switch state.Phase {
	case docMarkdownPlanBlockStart:
		line, err := compiler.line()
		if err != nil {
			return err
		}
		if line == nil {
			state.Phase = docMarkdownPlanFinish
			return nil
		}
		if line.Kind == docMarkdownLineBlank {
			return nil
		}
		state.Block = docMarkdownPlanBlock{From: line.From, To: line.To, Breaks: 2}
		state.BlockStarted, state.PendingSpaceFrom, state.PendingSpaceTo = false, -1, 0
		switch line.Kind {
		case docMarkdownLineFence:
			state.Block.From, state.Block.To, state.Block.Breaks = min(line.Next, state.Input.Bytes), min(line.Next, state.Input.Bytes), 1
			state.Phase = docMarkdownPlanFence
		case docMarkdownLineHeading, docMarkdownLineList:
			state.Block.From, state.Block.Breaks, state.Block.Collapse = line.ContentFrom, 1, true
			compiler.beginInline()
		default:
			state.Phase = docMarkdownPlanParagraph
		}
		state.PendingBreaks = max(state.PendingBreaks, state.Block.Breaks)
	case docMarkdownPlanParagraph, docMarkdownPlanFence:
		line, err := compiler.line()
		if err != nil {
			return err
		}
		if state.Phase == docMarkdownPlanParagraph {
			if line != nil && line.Kind == docMarkdownLinePlain {
				state.Block.To = line.To
				return nil
			}
			state.PendingLine = line
			compiler.beginInline()
		} else if line != nil && line.Kind != docMarkdownLineFence {
			state.Block.To = line.To
		} else {
			state.WriteFrom, state.WriteTo, state.ReturnInline = state.Block.From, state.Block.To, false
			state.Phase = docMarkdownPlanCopy
		}
	case docMarkdownPlanInline:
		return compiler.inline()
	case docMarkdownPlanCopy:
		if state.WriteFrom == state.WriteTo {
			state.Phase = docMarkdownPlanBlockEnd
			if state.ReturnInline {
				state.Phase = docMarkdownPlanInline
			}
			return nil
		}
		character, width, err := compiler.character(state.WriteFrom, 0)
		if err != nil {
			return err
		}
		if width > state.WriteTo-state.WriteFrom {
			return ErrIntegrity
		}
		if err := compiler.content(character, state.WriteFrom, state.WriteFrom+width); err != nil {
			return err
		}
		state.WriteFrom += width
	case docMarkdownPlanBlockEnd:
		if err := compiler.flushCopy(); err != nil {
			return err
		}
		state.PendingSpaceFrom, state.PendingSpaceTo = -1, 0
		state.PendingBreaks = max(state.PendingBreaks, state.Block.Breaks)
		state.Phase = docMarkdownPlanBlockStart
	case docMarkdownPlanFinish:
		if err := compiler.flushCopy(); err != nil {
			return err
		}
		if err := compiler.flushPage(); err != nil {
			return err
		}
		state.Phase = docMarkdownPlanDone
	default:
		return ErrIntegrity
	}
	return nil
}

func (compiler *docMarkdownPlanCompiler) beginInline() {
	state := &compiler.state
	state.Phase, state.InlineCursor = docMarkdownPlanInline, state.Block.From
	state.NextDelimiter = [4]int{-1, -1, -1, -1}
	state.LookupKind, state.LookupStart, state.LookupNext = 0, 0, 0
}

func (compiler *docMarkdownPlanCompiler) lookup(kind, start int) (int, error) {
	state := &compiler.state
	if state.NextDelimiter[kind-1] >= start {
		return state.NextDelimiter[kind-1], nil
	}
	if state.LookupKind == 0 {
		state.LookupKind, state.LookupStart, state.LookupNext = kind, start, start
	}
	if state.LookupKind != kind || state.LookupStart != start {
		return 0, ErrIntegrity
	}
	for state.LookupNext < state.Block.To {
		character, width, err := compiler.character(state.LookupNext, kind)
		if err != nil {
			return 0, err
		}
		if kind == 1 && character == '*' || kind == 2 && character == '`' || kind == 3 && character == ']' || kind == 4 && (character == ')' || docMarkdownWhitespace(character)) {
			break
		}
		state.LookupNext += width
	}
	position := state.LookupNext
	state.NextDelimiter[kind-1] = position
	state.LookupKind, state.LookupStart, state.LookupNext = 0, 0, 0
	return position, nil
}

func (compiler *docMarkdownPlanCompiler) prefix(position int, prefix string, slot int) (bool, error) {
	if position < compiler.state.Block.From || position > compiler.state.Block.To || len(prefix) > compiler.state.Block.To-position {
		return false, nil
	}
	raw, err := compiler.read(position, len(prefix), slot)
	return string(raw) == prefix, err
}

func (compiler *docMarkdownPlanCompiler) inline() error {
	state := &compiler.state
	if state.InlineCursor == state.Block.To {
		state.Phase = docMarkdownPlanBlockEnd
		return nil
	}
	character, width, err := compiler.character(state.InlineCursor, 0)
	if err != nil {
		return err
	}
	from, to, end := state.InlineCursor, state.InlineCursor, state.InlineCursor
	switch character {
	case '*':
		strong, err := compiler.prefix(state.InlineCursor, "**", 0)
		if err != nil {
			return err
		}
		if strong {
			from = state.InlineCursor + 2
			next, err := compiler.lookup(1, from)
			if err != nil {
				return err
			}
			closing, err := compiler.prefix(next, "**", 1)
			if err != nil {
				return err
			}
			if next > from && closing {
				to, end = next, next+2
			}
		}
	case '`':
		from = state.InlineCursor + 1
		next, err := compiler.lookup(2, from)
		if err != nil {
			return err
		}
		if next > from && next < state.Block.To {
			to, end = next, next+1
		}
	case '[':
		from = state.InlineCursor + 1
		next, err := compiler.lookup(3, from)
		if err != nil {
			return err
		}
		opening, err := compiler.prefix(next, "](", 3)
		if err != nil {
			return err
		}
		if next > from && opening {
			body := -1
			for _, protocol := range []string{"https://", "http://"} {
				matched, err := compiler.prefix(next+2, protocol, 3)
				if err != nil {
					return err
				}
				if matched {
					body = next + 2 + len(protocol)
					break
				}
			}
			if body >= 0 {
				terminator, err := compiler.lookup(4, body)
				if err != nil {
					return err
				}
				closing, err := compiler.prefix(terminator, ")", 4)
				if err != nil {
					return err
				}
				if terminator > body && closing {
					to, end = next, terminator+1
				}
			}
		}
	}
	if end > state.InlineCursor {
		state.WriteFrom, state.WriteTo, state.ReturnInline = from, to, true
		state.InlineCursor, state.Phase = end, docMarkdownPlanCopy
		return nil
	}
	if err := compiler.content(character, state.InlineCursor, state.InlineCursor+width); err != nil {
		return err
	}
	state.InlineCursor += width
	return nil
}

func (compiler *docMarkdownPlanCompiler) content(character rune, from, to int) error {
	state := &compiler.state
	if state.Block.Collapse && (character == ' ' || character == '\t' || character == '\r' || character == '\n') {
		if state.BlockStarted {
			if state.PendingSpaceFrom < 0 {
				state.PendingSpaceFrom = from
			}
			state.PendingSpaceTo = to
		}
		return nil
	}
	if !state.BlockStarted {
		if state.TextBytes > 0 {
			if err := compiler.synthetic(state.LastSourceEnd, from, state.PendingBreaks); err != nil {
				return err
			}
		} else {
			state.LeadingBreaks = state.PendingBreaks
		}
		state.BlockStarted, state.PendingBreaks = true, 0
	}
	if state.PendingSpaceFrom >= 0 {
		if err := compiler.space(state.PendingSpaceFrom, state.PendingSpaceTo, " "); err != nil {
			return err
		}
		state.PendingSpaceFrom, state.PendingSpaceTo = -1, 0
	}
	if state.CopyFrom >= 0 && state.CopyTo != from {
		if err := compiler.flushCopy(); err != nil {
			return err
		}
	}
	if to-from > MaxConversationDocumentBytes-state.TextBytes {
		return ErrDocFindDisplayUnavailable
	}
	if state.CopyFrom < 0 {
		state.CopyFrom = from
	}
	state.CopyTo, state.LastSourceEnd = to, to
	state.TextBytes += to - from
	return nil
}

func (compiler *docMarkdownPlanCompiler) synthetic(from, to, breaks int) error {
	text := "\n"
	if breaks == 2 {
		text = "\n\n"
	} else if breaks != 1 {
		return ErrIntegrity
	}
	return compiler.space(from, to, text)
}

func (compiler *docMarkdownPlanCompiler) space(from, to int, text string) error {
	if err := compiler.flushCopy(); err != nil {
		return err
	}
	if len(text) > MaxConversationDocumentBytes-compiler.state.TextBytes {
		return ErrDocFindDisplayUnavailable
	}
	compiler.state.TextBytes += len(text)
	compiler.state.LastSourceEnd = to
	return compiler.append(DocMarkdownPlanOperation{From: from, To: to, Text: text})
}

func (compiler *docMarkdownPlanCompiler) flushCopy() error {
	state := &compiler.state
	if state.CopyFrom < 0 {
		return nil
	}
	operation := DocMarkdownPlanOperation{From: state.CopyFrom, To: state.CopyTo}
	state.CopyFrom, state.CopyTo = -1, 0
	return compiler.append(operation)
}

func (compiler *docMarkdownPlanCompiler) append(operation DocMarkdownPlanOperation) error {
	if operation.Validate(compiler.state.Input) != nil {
		return ErrIntegrity
	}
	compiler.state.Operations = append(compiler.state.Operations, operation)
	compiler.state.OperationCount++
	compiler.operations++
	if len(compiler.state.Operations) == DocMarkdownPlanOperations {
		return compiler.flushPage()
	}
	return nil
}

func (compiler *docMarkdownPlanCompiler) flushPage() error {
	state := &compiler.state
	if len(state.Operations) == 0 {
		return nil
	}
	page := DocMarkdownPlanPage{Version: 1, Input: state.Input.identity(), Index: state.Directory.Count, From: state.PageFrom, Operations: state.Operations}
	raw, err := page.Encode(state.Input)
	if err != nil {
		return err
	}
	directory, objects, err := state.Directory.Append(compiler.ctx, HashContent(raw))
	if err != nil {
		return err
	}
	for _, operation := range state.Operations {
		state.PageFrom += operation.TextBytes()
	}
	state.Directory, state.Operations = directory, nil
	compiler.objects = append(compiler.objects, objects...)
	compiler.objects = append(compiler.objects, DocRangeObject{Hash: HashContent(raw), Bytes: raw})
	return nil
}
