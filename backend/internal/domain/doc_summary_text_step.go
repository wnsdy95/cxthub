package domain

import (
	"context"
	"strings"
	"unicode/utf8"
)

type DocSummaryTextStep struct {
	State     DocSummaryTextState
	Objects   []DocRangeObject
	ReadBytes int
}

type docSummaryInputReader struct {
	source DocSummaryTextSource
	load   func(context.Context, ContentHash) ([]byte, error)
	index  int
	page   []DocMessageInput
}

func (reader *docSummaryInputReader) block(ctx context.Context, index int) (DocMessageInput, error) {
	if err := ctx.Err(); err != nil {
		return DocMessageInput{}, err
	}
	if index < 0 || index >= reader.source.Message.Count {
		return DocMessageInput{}, ErrIntegrity
	}
	ordinal := index / DocMessageInputPageEntries
	if reader.page == nil || reader.index != ordinal {
		page, err := ReadDocMessageInputs(ctx, reader.source.Message, ordinal, reader.load)
		if err != nil {
			return DocMessageInput{}, err
		}
		reader.page, reader.index = page, ordinal
	}
	return reader.page[index%DocMessageInputPageEntries], nil
}

func StepDocSummaryText(ctx context.Context, source DocSummaryTextSource, prior DocSummaryTextState, loadRaw func(context.Context, int, int) ([]byte, error), loadObject func(context.Context, ContentHash) ([]byte, error)) (result DocSummaryTextStep, failure error) {
	defer func() {
		if err := ctx.Err(); err != nil {
			result, failure = DocSummaryTextStep{}, err
		}
	}()
	if err := ctx.Err(); err != nil {
		return DocSummaryTextStep{}, err
	}
	if prior.Validate(source) != nil || prior.Done || loadRaw == nil || loadObject == nil {
		return DocSummaryTextStep{}, ErrIntegrity
	}
	inputs := docSummaryInputReader{source: source, load: loadObject}
	var hashTail *DocDisplayPage
	previous, err := loadDocSummaryTextCheckpoint(ctx, source, prior, &inputs, &hashTail)
	if err != nil {
		return DocSummaryTextStep{}, err
	}
	encoded, err := prior.Encode(source)
	if err != nil {
		return DocSummaryTextStep{}, err
	}
	next, err := DecodeDocSummaryTextState(source, HashContent(encoded), encoded)
	if err != nil {
		return DocSummaryTextStep{}, err
	}
	page := DocDisplayPage{Version: 4, Source: prior.Source, Index: prior.Directory.Count, From: prior.PageFrom}
	var output strings.Builder
	if prior.Pending != "" {
		page.Runs = append([]DocDisplayRun(nil), previous.Runs...)
		output.WriteString(previous.Text)
	}
	initialBytes := output.Len()
	forcedSeal := false
	encodedBytes := 0
	for _, character := range output.String() {
		encodedBytes += docDisplayEncodedRuneBytes(character)
	}
	space := func() bool {
		return output.Len() <= DocDisplayPageTextBytes-utf8.UTFMax && len(page.Runs) < DocDisplayPageRuns && encodedBytes+docDisplayEncodedHeaderBytes+(len(page.Runs)+1)*docDisplayEncodedRunBytes+24 <= MaxDocDisplayPageBytes
	}
	appendPart := func(text string, runs []DocDisplayRun) {
		base := output.Len()
		for index, run := range runs {
			start := 0
			if index > 0 {
				start = runs[index-1].To
			}
			for _, character := range text[start:run.To] {
				encodedBytes += docDisplayEncodedRuneBytes(character)
			}
			run.To += base
			if run.Linear && len(page.Runs) > 0 && page.Runs[len(page.Runs)-1].Linear && page.Runs[len(page.Runs)-1].SourceTo == run.SourceFrom {
				last := &page.Runs[len(page.Runs)-1]
				last.To, last.SourceTo = run.To, run.SourceTo
			} else {
				page.Runs = append(page.Runs, run)
			}
		}
		output.WriteString(text)
	}
	for next.Block < source.Message.Count && next.Block-prior.Block < DocSummaryTextStepSlots && result.ReadBytes <= DocDisplayStepSourceBytes-24 && space() {
		input, err := inputs.block(ctx, next.Block)
		if err != nil {
			return DocSummaryTextStep{}, err
		}
		selected, err := input.TextSource()
		if err != nil {
			return DocSummaryTextStep{}, err
		}
		if !next.Joined {
			if next.Block > 0 {
				anchor := input.Anchor()
				if selected != nil {
					length := min(12, selected.Token.To-selected.Token.From-1)
					raw, err := loadRaw(ctx, selected.Token.From+1, length)
					if err != nil {
						return DocSummaryTextStep{}, err
					}
					if len(raw) != length {
						return DocSummaryTextStep{}, ErrIntegrity
					}
					result.ReadBytes += length
					if raw[0] != '"' {
						_, width, _, err := decodeDocDisplayStringRune(raw)
						if err != nil {
							return DocSummaryTextStep{}, err
						}
						anchor = DocFindSpan{From: selected.Token.From + 1, To: selected.Token.From + 1 + width}
					}
				}
				appendPart("\n", []DocDisplayRun{{To: 1, SourceFrom: anchor.From, SourceTo: anchor.To}})
			}
			next.Joined = true
		}
		if selected == nil {
			next.Block++
			next.Next, next.Joined = 0, false
			continue
		}
		if !space() || result.ReadBytes > DocDisplayStepSourceBytes-24 {
			break
		}
		maxRuns := min(64, DocDisplayPageRuns-len(page.Runs))
		maxBytes := min(DocDisplayPageTextBytes-output.Len(), (MaxDocDisplayPageBytes-docDisplayEncodedHeaderBytes-(len(page.Runs)+maxRuns)*docDisplayEncodedRunBytes-encodedBytes)/6)
		if maxBytes < utf8.UTFMax {
			forcedSeal = true
			break
		}
		if next.Next == 0 {
			next.Next = selected.Token.From
		}
		decoded, err := decodeDocJSONStringPage(ctx, docJSONStringPageRequest{Version: 4, Source: next.Source, Token: selected.Token, Next: next.Next, Index: page.Index, From: page.From + output.Len(), MaxBytes: maxBytes, MaxRuns: maxRuns, MaxRead: DocDisplayStepSourceBytes - result.ReadBytes}, loadRaw)
		if err != nil {
			return DocSummaryTextStep{}, err
		}
		result.ReadBytes += decoded.ReadBytes
		next.Next = decoded.Next
		if decoded.Page != nil {
			appendPart(decoded.Page.Text, decoded.Page.Runs)
		}
		if decoded.Done {
			next.Block++
			next.Next, next.Joined = 0, false
		}
	}
	next.TextBytes = page.From + output.Len()
	next.Done = next.Block == source.Message.Count
	page.Text = output.String()
	if len(page.Text) > 0 {
		raw, err := encodeDocSummaryTextPage(source, page)
		if err != nil {
			return DocSummaryTextStep{}, err
		}
		result.Objects = append(result.Objects, DocRangeObject{Hash: HashContent(raw), Bytes: raw})
		if next.Done || forcedSeal || !space() {
			if !next.Done && utf8.RuneCountInString(page.Text) < DocDisplayPageRuns {
				return DocSummaryTextStep{}, ErrIntegrity
			}
			if err := sealDocSummaryTextPage(ctx, &next, page, HashContent(raw), &result.Objects); err != nil {
				return DocSummaryTextStep{}, err
			}
		} else {
			next.Pending = HashContent(raw)
		}
	}
	hash, err := advanceDocPreparedTextHash(docPreparedTextHashState{Next: prior.HashNext, State: prior.HashState}, prior.TextBytes, next.Done, []byte(page.Text[initialBytes:]), hashTail)
	if err != nil {
		return DocSummaryTextStep{}, err
	}
	next.HashNext, next.HashState, next.Hash = hash.Next, hash.State, hash.Hash
	if next.Done {
		if err := flushDocSummaryTextLookup(ctx, &next, &result.Objects); err != nil {
			return DocSummaryTextStep{}, err
		}
	}
	if next.Validate(source) != nil || next.Block == prior.Block && next.Next == prior.Next && next.Joined == prior.Joined && next.TextBytes == prior.TextBytes {
		return DocSummaryTextStep{}, ErrIntegrity
	}
	result.State = next
	return result, nil
}

func sealDocSummaryTextPage(ctx context.Context, state *DocSummaryTextState, page DocDisplayPage, hash ContentHash, objects *[]DocRangeObject) error {
	directory, produced, err := state.Directory.Append(ctx, hash)
	if err != nil {
		return err
	}
	*objects = append(*objects, produced...)
	state.Directory, state.Pending = directory, ""
	for bucket := (state.PageFrom + DocDecodedTextLookupStride - 1) / DocDecodedTextLookupStride; bucket < (state.TextBytes+DocDecodedTextLookupStride-1)/DocDecodedTextLookupStride; bucket++ {
		state.LookupEntries = append(state.LookupEntries, page.Index)
		if len(state.LookupEntries) == DocDecodedTextLookupEntries {
			if err := flushDocSummaryTextLookup(ctx, state, objects); err != nil {
				return err
			}
		}
	}
	state.PageFrom = state.TextBytes
	return nil
}

func flushDocSummaryTextLookup(ctx context.Context, state *DocSummaryTextState, objects *[]DocRangeObject) error {
	if len(state.LookupEntries) == 0 {
		return nil
	}
	page := DocDecodedTextLookupPage{Version: 1, Source: state.Source, Index: state.LookupDirectory.Count, Pages: state.LookupEntries}
	raw, err := page.encodeForSource(state.Source)
	if err != nil {
		return err
	}
	directory, produced, err := state.LookupDirectory.Append(ctx, HashContent(raw))
	if err != nil {
		return err
	}
	*objects = append(*objects, produced...)
	*objects = append(*objects, DocRangeObject{Hash: HashContent(raw), Bytes: raw})
	state.LookupDirectory, state.LookupEntries = directory, nil
	return nil
}

func loadDocSummaryTextCheckpoint(ctx context.Context, source DocSummaryTextSource, state DocSummaryTextState, inputs *docSummaryInputReader, tail **DocDisplayPage) (*DocDisplayPage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if state.Validate(source) != nil || inputs.load == nil {
		return nil, ErrIntegrity
	}
	var previous *DocDisplayPage
	var hashTail *DocDisplayPage
	var preceding *DocDisplayPage
	if state.Directory.Count > 0 {
		root, staged, err := state.Directory.Finish(ctx)
		if err != nil {
			return nil, err
		}
		load := func(ctx context.Context, hash ContentHash) ([]byte, error) {
			for _, object := range staged {
				if object.Hash == hash {
					return object.Bytes, nil
				}
			}
			return inputs.load(ctx, hash)
		}
		hash, err := ResolveDocRangeLeaf(ctx, root, state.Directory.Count-1, load)
		if err != nil {
			return nil, err
		}
		raw, err := load(ctx, hash)
		if err != nil {
			return nil, err
		}
		page, err := DecodeDocSummaryTextPage(source, hash, raw)
		if err != nil || page.Index != state.Directory.Count-1 || page.From+len(page.Text) != state.PageFrom || !state.Done && utf8.RuneCountInString(page.Text) < DocDisplayPageRuns {
			return nil, ErrIntegrity
		}
		previous = &page
		hashTail = &page
	}
	if state.Pending != "" {
		preceding = previous
		raw, err := inputs.load(ctx, state.Pending)
		if err != nil {
			return nil, err
		}
		page, err := DecodeDocSummaryTextPage(source, state.Pending, raw)
		if err != nil || page.Index != state.Directory.Count || page.From != state.PageFrom || page.From+len(page.Text) != state.TextBytes || previous != nil && !adjacentDocSummaryTextPages(*previous, page) {
			return nil, ErrIntegrity
		}
		if state.HashNext < page.From {
			if previous == nil || state.HashNext < previous.From {
				return nil, ErrIntegrity
			}
			hashTail = &DocDisplayPage{From: state.HashNext, Text: previous.Text[state.HashNext-previous.From:] + page.Text}
		} else {
			hashTail = &page
		}
		previous = &page
	}
	selectedBlock := state.Block
	if state.Next == 0 && !state.Joined {
		selectedBlock--
	}
	if previous != nil {
		if selectedBlock < 0 {
			return nil, ErrIntegrity
		}
		input, err := inputs.block(ctx, selectedBlock)
		if err != nil {
			return nil, err
		}
		last := previous.Runs[len(previous.Runs)-1]
		if last.SourceFrom < input.Anchor().From || last.SourceTo > input.Anchor().To || state.Next > 0 && last.SourceTo != state.Next {
			return nil, ErrIntegrity
		}
	}
	if state.Block < source.Message.Count {
		input, err := inputs.block(ctx, state.Block)
		if err != nil {
			return nil, err
		}
		selected, err := input.TextSource()
		if err != nil || state.Next > 0 && (selected == nil || state.Next <= selected.Token.From || state.Next >= selected.Token.To) {
			return nil, ErrIntegrity
		}
		if state.Joined && state.Next == 0 {
			if previous == nil || validateDocSummaryJoinCheckpoint(input, selected, *previous, preceding) != nil {
				return nil, ErrIntegrity
			}
		}
	}
	if state.TextBytes == 0 && state.Block > 0 {
		first, err := inputs.block(ctx, 0)
		if err != nil {
			return nil, err
		}
		selected, err := first.TextSource()
		if err != nil || selected != nil && selected.Token.To-selected.Token.From != 2 {
			return nil, ErrIntegrity
		}
	}
	if tail != nil {
		*tail = hashTail
	}
	return previous, ctx.Err()
}

func validateDocSummaryJoinCheckpoint(input DocMessageInput, selected *DocDecodedTextSource, page DocDisplayPage, preceding *DocDisplayPage) error {
	last := page.Runs[len(page.Runs)-1]
	start := 0
	if len(page.Runs) > 1 {
		before := page.Runs[len(page.Runs)-2]
		start = before.To
		if before.SourceTo > input.Anchor().From {
			return ErrIntegrity
		}
	} else if preceding != nil && preceding.Runs[len(preceding.Runs)-1].SourceTo > input.Anchor().From {
		return ErrIntegrity
	}
	if last.Linear || page.Text[start:] != "\n" {
		return ErrIntegrity
	}
	if selected == nil || selected.Token.To-selected.Token.From == 2 {
		if last.SourceFrom != input.Anchor().From || last.SourceTo != input.Anchor().To {
			return ErrIntegrity
		}
	} else if last.SourceFrom != selected.Token.From+1 || last.SourceTo <= last.SourceFrom || last.SourceTo-last.SourceFrom > 12 || last.SourceTo >= selected.Token.To {
		return ErrIntegrity
	}
	return nil
}

func ValidateDocSummaryTextCheckpoint(ctx context.Context, source DocSummaryTextSource, state DocSummaryTextState, load func(context.Context, ContentHash) ([]byte, error)) (failure error) {
	defer func() {
		if err := ctx.Err(); err != nil {
			failure = err
		}
	}()
	_, failure = loadDocSummaryTextCheckpoint(ctx, source, state, &docSummaryInputReader{source: source, load: load}, nil)
	return failure
}

func FinishDocSummaryText(ctx context.Context, source DocSummaryTextSource, state DocSummaryTextState, load func(context.Context, ContentHash) ([]byte, error)) (result DocSummaryTextGeneration, objects []DocRangeObject, failure error) {
	defer func() {
		if err := ctx.Err(); err != nil {
			result, objects, failure = DocSummaryTextGeneration{}, nil, err
		}
	}()
	if err := ctx.Err(); err != nil {
		return DocSummaryTextGeneration{}, nil, err
	}
	if state.Validate(source) != nil || !state.Done {
		return DocSummaryTextGeneration{}, nil, ErrIntegrity
	}
	if err := ValidateDocSummaryTextCheckpoint(ctx, source, state, load); err != nil {
		return DocSummaryTextGeneration{}, nil, err
	}
	pages, produced, err := state.Directory.Finish(ctx)
	if err != nil {
		return DocSummaryTextGeneration{}, nil, err
	}
	lookup, indexed, err := state.LookupDirectory.Finish(ctx)
	if err != nil {
		return DocSummaryTextGeneration{}, nil, err
	}
	result = DocSummaryTextGeneration{Version: 1, Source: source, Hash: state.Hash, TextBytes: state.TextBytes, Pages: pages, Lookup: lookup}
	if result.Validate() != nil {
		return DocSummaryTextGeneration{}, nil, ErrIntegrity
	}
	return result, append(produced, indexed...), nil
}
