package domain

import (
	"bytes"
	"context"
	"strings"
	"unicode/utf8"
)

type DocToolStringStep struct {
	State     DocToolStringState
	Objects   []DocRangeObject
	ReadBytes int
	page      DocToolStringPage
}

func docToolStringRune(raw []byte, mode DocToolStringMode) (string, int, bool, error) {
	character, width, _, err := decodeDocDisplayStringRune(raw)
	if err != nil {
		return "", 0, false, err
	}
	text := string(character)
	if mode == DocToolStringQuoted {
		switch character {
		case '"':
			text = `\"`
		case '\\':
			text = `\\`
		case '\b':
			text = `\b`
		case '\f':
			text = `\f`
		case '\n':
			text = `\n`
		case '\r':
			text = `\r`
		case '\t':
			text = `\t`
		default:
			if character < 0x20 {
				const hex = "0123456789abcdef"
				text = string([]byte{'\\', 'u', '0', '0', hex[character>>4], hex[character&15]})
			} else if character == utf8.RuneError && width == 6 && raw[1] == 'u' {
				unit, err := docDisplayHexRune(raw[:width])
				if err != nil {
					return "", 0, false, err
				}
				if unit >= 0xd800 && unit <= 0xdfff {
					text = strings.ToLower(string(raw[:width]))
				}
			}
		}
	} else if character == '\n' {
		text = "\n" + docToolStringPrefix(mode)
	}
	return text, width, text == string(raw[:width]), nil
}

func docToolStringPrefix(mode DocToolStringMode) string {
	if mode == DocToolStringRemove {
		return "− "
	}
	return "+ "
}

func StepDocToolString(ctx context.Context, source DocToolStringSource, prior DocToolStringState, loadRaw func(context.Context, int, int) ([]byte, error), loadObject func(context.Context, ContentHash) ([]byte, error)) (result DocToolStringStep, failure error) {
	defer func() {
		if err := ctx.Err(); err != nil {
			failure = err
		}
		if failure != nil {
			result = DocToolStringStep{}
		}
	}()
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if prior.Validate(source) != nil || prior.Done || loadRaw == nil {
		return result, ErrIntegrity
	}
	if err := ValidateDocToolStringCheckpoint(ctx, source, prior, loadObject); err != nil {
		return result, err
	}
	input := docDisplayStringInput{ctx: ctx, load: loadRaw, end: source.Token.To, base: prior.Next, readTo: prior.Next, maxRead: DocDisplayStepSourceBytes - 1}
	page := DocToolStringPage{Version: 1, Source: prior.Source, RawFrom: prior.Next, Display: DocDisplayPage{Version: 5, Source: prior.Source, Index: prior.Directory.Count, From: prior.TextBytes}}
	var text strings.Builder
	encoded := 0
	emit := func(value string, anchor DocFindSpan, linear bool) {
		text.WriteString(value)
		for _, character := range value {
			encoded += docDisplayEncodedRuneBytes(character)
		}
		last := len(page.Display.Runs) - 1
		if last >= 0 {
			run := &page.Display.Runs[last]
			if linear && run.Linear && run.SourceTo == anchor.From || !linear && !run.Linear && run.SourceFrom == anchor.From && run.SourceTo == anchor.To {
				run.To = text.Len()
				if linear {
					run.SourceTo = anchor.To
				}
				return
			}
		}
		page.Display.Runs = append(page.Display.Runs, DocDisplayRun{To: text.Len(), SourceFrom: anchor.From, SourceTo: anchor.To, Linear: linear})
	}
	if prior.Next == source.Token.From {
		ready, err := input.ready(DocDisplayPageTextBytes)
		if err != nil {
			return result, err
		}
		if !ready || input.raw[input.position] != '"' {
			return result, ErrIntegrity
		}
		anchor := DocFindSpan{From: input.offset(), To: input.offset() + 1}
		if source.Mode == DocToolStringQuoted {
			emit("\"", anchor, true)
		} else {
			emit(docToolStringPrefix(source.Mode), anchor, false)
		}
		input.position++
	}
	for text.Len() <= DocDisplayPageTextBytes-12 && len(page.Display.Runs) < DocDisplayPageRuns-1 && encoded+docDisplayEncodedHeaderBytes+(len(page.Display.Runs)+2)*docDisplayEncodedRunBytes+72 <= MaxDocDisplayPageBytes {
		ready, err := input.ready(min(DocDisplayPageTextBytes-text.Len(), (DocDisplayPageRuns-len(page.Display.Runs))*12))
		if err != nil {
			return result, err
		}
		if !ready {
			break
		}
		raw := input.raw[input.position:]
		if raw[0] == '"' {
			if input.offset() != source.Token.To-1 {
				return result, ErrIntegrity
			}
			break
		}
		value, width, linear, err := docToolStringRune(raw, source.Mode)
		if err != nil || input.offset()+width >= source.Token.To {
			return result, ErrIntegrity
		}
		emit(value, DocFindSpan{From: input.offset(), To: input.offset() + width}, linear)
		input.position += width
	}
	next := prior
	if input.offset() == source.Token.To-1 {
		var closing []byte
		if input.position < len(input.raw) {
			closing = input.raw[input.position : input.position+1]
		} else {
			var err error
			closing, err = loadRaw(ctx, input.offset(), 1)
			if err != nil {
				return result, err
			}
			input.used++
		}
		if !bytes.Equal(closing, []byte{'"'}) {
			return result, ErrIntegrity
		}
		if source.Mode == DocToolStringQuoted {
			emit("\"", DocFindSpan{From: input.offset(), To: input.offset() + 1}, true)
		}
		input.position++
		next.Done = true
	}
	page.RawTo, page.Display.Text = input.offset(), text.String()
	raw, err := page.Encode(source)
	if err != nil || page.RawTo <= prior.Next || len(page.Display.Text) == 0 || len(page.Display.Text) > MaxConversationDocumentBytes-prior.TextBytes {
		return result, ErrIntegrity
	}
	directory, objects, err := prior.Directory.Append(ctx, HashContent(raw))
	if err != nil {
		return result, err
	}
	objects = append(objects, DocRangeObject{Hash: HashContent(raw), Bytes: raw})
	next.Directory, next.Next, next.TextBytes = directory, page.RawTo, prior.TextBytes+len(page.Display.Text)
	if next.Validate(source) != nil {
		return result, ErrIntegrity
	}
	return DocToolStringStep{State: next, Objects: objects, ReadBytes: input.used, page: page}, nil
}

func ValidateDocToolStringCheckpoint(ctx context.Context, source DocToolStringSource, state DocToolStringState, load func(context.Context, ContentHash) ([]byte, error)) (failure error) {
	defer func() {
		if err := ctx.Err(); err != nil {
			failure = err
		}
	}()
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
	hash, err := ResolveDocRangeLeaf(ctx, root, root.Count-1, selected)
	if err != nil {
		return err
	}
	raw, err := selected(ctx, hash)
	if err != nil {
		return err
	}
	page, err := DecodeDocToolStringPage(source, hash, raw)
	if err != nil || page.Display.Index != root.Count-1 || page.Display.From+len(page.Display.Text) != state.TextBytes || page.RawTo != state.Next {
		return ErrIntegrity
	}
	return nil
}

func FinishDocToolString(ctx context.Context, source DocToolStringSource, state DocToolStringState, load func(context.Context, ContentHash) ([]byte, error)) (result DocToolStringPlan, objects []DocRangeObject, failure error) {
	defer func() {
		if err := ctx.Err(); err != nil {
			failure = err
		}
		if failure != nil {
			result, objects = DocToolStringPlan{}, nil
		}
	}()
	if err := ValidateDocToolStringCheckpoint(ctx, source, state, load); err != nil {
		return result, nil, err
	}
	if !state.Done {
		return result, nil, ErrIntegrity
	}
	root, objects, err := state.Directory.Finish(ctx)
	if err != nil {
		return result, nil, err
	}
	plan := DocToolStringPlan{Version: 1, Source: source, Pages: root, TextBytes: state.TextBytes}
	if plan.Validate() != nil {
		return result, nil, ErrIntegrity
	}
	return plan, objects, nil
}
