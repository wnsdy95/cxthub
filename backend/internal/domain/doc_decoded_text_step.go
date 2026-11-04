package domain

import (
	"context"
	"crypto/sha256"
	"encoding"
	"encoding/hex"
)

type DocDecodedTextStep struct {
	State     DocDecodedTextState
	Page      *DocDisplayPage
	Objects   []DocRangeObject
	ReadBytes int
}

func StepDocDecodedText(ctx context.Context, source DocDecodedTextSource, prior DocDecodedTextState, loadRaw func(context.Context, int, int) ([]byte, error), loadObject func(context.Context, ContentHash) ([]byte, error)) (result DocDecodedTextStep, failure error) {
	defer func() {
		if err := ctx.Err(); err != nil {
			result, failure = DocDecodedTextStep{}, err
		}
	}()
	if err := ctx.Err(); err != nil {
		return DocDecodedTextStep{}, err
	}
	if prior.Validate(source) != nil || prior.Done || loadRaw == nil {
		return DocDecodedTextStep{}, ErrIntegrity
	}
	previousPage, err := loadDocDecodedTextCheckpointPage(ctx, source, prior, loadObject)
	if err != nil {
		return DocDecodedTextStep{}, err
	}
	decoded, err := decodeDocJSONStringPage(ctx, docJSONStringPageRequest{Version: 3, Source: prior.Source, Token: source.Token, Next: prior.Next, Index: prior.Directory.Count, From: prior.TextBytes}, loadRaw)
	if err != nil {
		return DocDecodedTextStep{}, err
	}
	next := prior
	next.LookupEntries = append([]int(nil), prior.LookupEntries...)
	next.Next, next.Done = decoded.Next, decoded.Done
	var objects []DocRangeObject
	if decoded.Page != nil {
		raw, err := encodeDocDecodedTextPage(source, *decoded.Page)
		if err != nil {
			return DocDecodedTextStep{}, err
		}
		next.Directory, objects, err = prior.Directory.Append(ctx, HashContent(raw))
		if err != nil {
			return DocDecodedTextStep{}, err
		}
		objects = append(objects, DocRangeObject{Hash: HashContent(raw), Bytes: raw})
		next.TextBytes += len(decoded.Page.Text)
		for bucket := (prior.TextBytes + DocDecodedTextLookupStride - 1) / DocDecodedTextLookupStride; bucket < (next.TextBytes+DocDecodedTextLookupStride-1)/DocDecodedTextLookupStride; bucket++ {
			next.LookupEntries = append(next.LookupEntries, decoded.Page.Index)
			if len(next.LookupEntries) == DocDecodedTextLookupEntries {
				if err := flushDocDecodedTextLookup(ctx, source, &next, &objects); err != nil {
					return DocDecodedTextStep{}, err
				}
			}
		}
	}
	if err := advanceDocDecodedTextHash(prior, &next, decoded.Page, previousPage); err != nil {
		return DocDecodedTextStep{}, err
	}
	if next.Done {
		if err := flushDocDecodedTextLookup(ctx, source, &next, &objects); err != nil {
			return DocDecodedTextStep{}, err
		}
	}
	if next.Validate(source) != nil {
		return DocDecodedTextStep{}, ErrIntegrity
	}
	return DocDecodedTextStep{State: next, Page: decoded.Page, Objects: objects, ReadBytes: decoded.ReadBytes}, nil
}

func flushDocDecodedTextLookup(ctx context.Context, source DocDecodedTextSource, state *DocDecodedTextState, objects *[]DocRangeObject) error {
	if len(state.LookupEntries) == 0 {
		return nil
	}
	page := DocDecodedTextLookupPage{Version: 1, Source: state.Source, Index: state.LookupDirectory.Count, Pages: state.LookupEntries}
	raw, err := page.Encode(source)
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

func ValidateDocDecodedTextCheckpoint(ctx context.Context, source DocDecodedTextSource, state DocDecodedTextState, load func(context.Context, ContentHash) ([]byte, error)) error {
	_, err := loadDocDecodedTextCheckpointPage(ctx, source, state, load)
	return err
}

func loadDocDecodedTextCheckpointPage(ctx context.Context, source DocDecodedTextSource, state DocDecodedTextState, load func(context.Context, ContentHash) ([]byte, error)) (result *DocDisplayPage, failure error) {
	defer func() {
		if err := ctx.Err(); err != nil {
			result, failure = nil, err
		}
	}()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if state.Validate(source) != nil {
		return nil, ErrIntegrity
	}
	if state.Directory.Count == 0 {
		return nil, nil
	}
	if load == nil {
		return nil, ErrIntegrity
	}
	root, staged, err := state.Directory.Finish(ctx)
	if err != nil {
		return nil, err
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
		return nil, err
	}
	raw, err := selected(ctx, hash)
	if err != nil {
		return nil, err
	}
	page, err := DecodeDocDecodedTextPage(source, hash, raw)
	if err != nil || page.Index != state.Directory.Count-1 || page.From+len(page.Text) != state.TextBytes || state.HashNext < page.From {
		return nil, ErrIntegrity
	}
	expectedEnd := state.Next
	if state.Done {
		expectedEnd--
	}
	if page.Runs[len(page.Runs)-1].SourceTo != expectedEnd {
		return nil, ErrIntegrity
	}
	return &page, nil
}

func advanceDocDecodedTextHash(prior DocDecodedTextState, next *DocDecodedTextState, page, previous *DocDisplayPage) error {
	var added []byte
	if page != nil {
		added = []byte(page.Text)
	}
	hash, err := advanceDocPreparedTextHash(docPreparedTextHashState{Next: prior.HashNext, State: prior.HashState}, prior.TextBytes, next.Done, added, previous)
	if err != nil {
		return err
	}
	next.HashNext, next.HashState, next.Hash = hash.Next, hash.State, hash.Hash
	return nil
}

type docPreparedTextHashState struct {
	Next  int
	State []byte
	Hash  ContentHash
}

func advanceDocPreparedTextHash(prior docPreparedTextHashState, textBytes int, done bool, added []byte, previous *DocDisplayPage) (docPreparedTextHashState, error) {
	if prior.Next != textBytes/sha256.BlockSize*sha256.BlockSize || !validDocFieldHashState(prior.State, prior.Next) || len(added) > DocDisplayPageTextBytes {
		return docPreparedTextHashState{}, ErrIntegrity
	}
	digest := sha256.New()
	if err := digest.(encoding.BinaryUnmarshaler).UnmarshalBinary(prior.State); err != nil {
		return docPreparedTextHashState{}, ErrIntegrity
	}
	var pending []byte
	if textBytes != prior.Next {
		if previous == nil || previous.From+len(previous.Text) != textBytes || prior.Next < previous.From || prior.Next-previous.From > len(previous.Text) {
			return docPreparedTextHashState{}, ErrIntegrity
		}
		pending = append(pending, previous.Text[prior.Next-previous.From:]...)
	}
	pending = append(pending, added...)
	length := len(pending)
	if !done {
		length = length / sha256.BlockSize * sha256.BlockSize
	}
	_, _ = digest.Write(pending[:length])
	next := docPreparedTextHashState{Next: prior.Next + length}
	if done {
		next.Hash = ContentHash("sha256:" + hex.EncodeToString(digest.Sum(nil)))
	} else {
		var err error
		next.State, err = digest.(encoding.BinaryMarshaler).MarshalBinary()
		if err != nil {
			return docPreparedTextHashState{}, err
		}
	}
	return next, nil
}
