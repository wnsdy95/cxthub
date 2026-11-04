package domain

import (
	"bytes"
	"context"
	"encoding/json"
)

const docCompositeAssemblyMaxInputs = 16

type docCompositeAssemblyCursor struct {
	Version int             `json:"version"`
	Plan    ContentHash     `json:"plan"`
	Parts   ContentHash     `json:"parts"`
	Part    int             `json:"part"`
	Child   json.RawMessage `json:"child"`
	From    int             `json:"from"`
}

func encodeDocCompositeAssemblyCursor(cursor docCompositeAssemblyCursor) (json.RawMessage, error) {
	raw, err := canonicalJSON(cursor)
	if err != nil || len(raw) > MaxDocDisplaySourceBytes {
		return nil, ErrIntegrity
	}
	return raw, nil
}

func newDocCompositeAssemblyInput(source DocDisplayProjectionSource, inputs []DocDisplayAssemblyInput) (DocDisplayAssemblyInput, error) {
	if source.Validate() != nil || len(inputs) > docCompositeAssemblyMaxInputs {
		return DocDisplayAssemblyInput{}, ErrIntegrity
	}
	children := append([]DocDisplayAssemblyInput(nil), inputs...)
	offsets := make([]int, len(children)+1)
	hashes := make([]ContentHash, len(children))
	bindings := make([][2]ContentHash, len(children))
	for index := range children {
		child := &children[index]
		if len(child.initial) == 0 || len(child.initial) > MaxDocDisplaySourceBytes {
			return DocDisplayAssemblyInput{}, ErrIntegrity
		}
		child.initial = bytes.Clone(child.initial)
		validation := *child
		validation.initial = bytes.Clone(child.initial)
		if validation.validate() != nil || child.source.EventHash != source.EventHash || child.source.EventBytes != source.EventBytes || child.source.TextBytes > source.TextBytes-offsets[index] {
			return DocDisplayAssemblyInput{}, ErrIntegrity
		}
		canonical, err := canonicalJSON(child.initial)
		if err != nil || !bytes.Equal(canonical, child.initial) {
			return DocDisplayAssemblyInput{}, ErrIntegrity
		}
		hashes[index], err = child.source.Hash()
		if err != nil {
			return DocDisplayAssemblyInput{}, err
		}
		bindings[index] = [2]ContentHash{hashes[index], HashContent(child.initial)}
		offsets[index+1] = offsets[index] + child.source.TextBytes
	}
	if offsets[len(children)] != source.TextBytes {
		return DocDisplayAssemblyInput{}, ErrIntegrity
	}
	hash, err := source.Hash()
	if err != nil {
		return DocDisplayAssemblyInput{}, err
	}
	bound, err := canonicalJSON(struct {
		Source   ContentHash      `json:"source"`
		Children [][2]ContentHash `json:"children"`
	}{Source: hash, Children: bindings})
	if err != nil {
		return DocDisplayAssemblyInput{}, err
	}
	parts := HashContent(bound)
	startPart := func(part int) docCompositeAssemblyCursor {
		for part < len(children) && children[part].source.TextBytes == 0 {
			part++
		}
		cursor := docCompositeAssemblyCursor{Version: 1, Plan: source.Plan, Parts: parts, Part: part, From: offsets[part]}
		if part < len(children) {
			cursor.Child = bytes.Clone(children[part].initial)
		}
		return cursor
	}
	for part := range children {
		if _, err := encodeDocCompositeAssemblyCursor(startPart(part)); err != nil {
			return DocDisplayAssemblyInput{}, err
		}
	}
	initial, err := encodeDocCompositeAssemblyCursor(startPart(0))
	if err != nil {
		return DocDisplayAssemblyInput{}, err
	}
	decode := func(raw json.RawMessage) (docCompositeAssemblyCursor, error) {
		var cursor docCompositeAssemblyCursor
		if len(raw) == 0 || len(raw) > MaxDocDisplaySourceBytes || json.Unmarshal(raw, &cursor) != nil || cursor.Version != 1 || cursor.Plan != source.Plan || cursor.Parts != parts || cursor.Part < 0 || cursor.Part > len(children) || cursor.From < 0 || cursor.From > source.TextBytes {
			return docCompositeAssemblyCursor{}, ErrIntegrity
		}
		canonical, err := encodeDocCompositeAssemblyCursor(cursor)
		if err != nil || !bytes.Equal(canonical, raw) {
			return docCompositeAssemblyCursor{}, ErrIntegrity
		}
		if cursor.Part == len(children) {
			if cursor.From != source.TextBytes || !bytes.Equal(cursor.Child, []byte("null")) {
				return docCompositeAssemblyCursor{}, ErrIntegrity
			}
			return cursor, nil
		}
		child := children[cursor.Part]
		position, err := child.cursor(bytes.Clone(cursor.Child))
		if err != nil || position < 0 || position >= child.source.TextBytes || cursor.From != offsets[cursor.Part]+position || position == 0 && !bytes.Equal(cursor.Child, child.initial) {
			return docCompositeAssemblyCursor{}, ErrIntegrity
		}
		return cursor, nil
	}
	input := DocDisplayAssemblyInput{source: source, initial: initial}
	input.cursor = func(raw json.RawMessage) (int, error) {
		cursor, err := decode(raw)
		if err != nil {
			return 0, err
		}
		return cursor.From, nil
	}
	input.read = func(ctx context.Context, raw json.RawMessage, maxBytes, maxRuns int, load func(context.Context, ContentHash) ([]byte, error)) (page DocDisplayPage, next json.RawMessage, failure error) {
		defer func() {
			if err := ctx.Err(); err != nil {
				failure = err
			}
			if failure != nil {
				page, next = DocDisplayPage{}, nil
			}
		}()
		if err := ctx.Err(); err != nil {
			return DocDisplayPage{}, nil, err
		}
		cursor, err := decode(raw)
		if err != nil || cursor.Part == len(children) || maxBytes <= 0 || maxBytes > DocDisplayPageTextBytes || maxRuns <= 0 || maxRuns > DocDisplayPageRuns || load == nil {
			return DocDisplayPage{}, nil, ErrIntegrity
		}
		child := children[cursor.Part]
		from := cursor.From
		position := cursor.From - offsets[cursor.Part]
		fragment, childNext, err := child.read(ctx, bytes.Clone(cursor.Child), maxBytes, maxRuns, newDocMappedObjectLoader(load))
		if err != nil {
			return DocDisplayPage{}, nil, err
		}
		if err := ctx.Err(); err != nil {
			return DocDisplayPage{}, nil, err
		}
		if len(childNext) == 0 || len(childNext) > MaxDocDisplaySourceBytes {
			return DocDisplayPage{}, nil, ErrIntegrity
		}
		canonical, err := canonicalJSON(childNext)
		if err != nil || !bytes.Equal(canonical, childNext) {
			return DocDisplayPage{}, nil, ErrIntegrity
		}
		consumed, err := child.cursor(bytes.Clone(childNext))
		if err != nil || fragment.Version != 2 || fragment.Source != hashes[cursor.Part] || fragment.From != position || len(fragment.Text) > maxBytes || len(fragment.Text) > child.source.TextBytes-position || len(fragment.Runs) > maxRuns || consumed != position+len(fragment.Text) || fragment.validateCoordinates(source.EventBytes, false) != nil {
			return DocDisplayPage{}, nil, ErrIntegrity
		}
		cursor.From += len(fragment.Text)
		cursor.Child = bytes.Clone(childNext)
		if consumed == child.source.TextBytes {
			cursor = startPart(cursor.Part + 1)
		}
		next, err = encodeDocCompositeAssemblyCursor(cursor)
		if err != nil {
			return DocDisplayPage{}, nil, err
		}
		if _, err := decode(next); err != nil {
			return DocDisplayPage{}, nil, err
		}
		page = fragment
		page.Source, page.From = hash, from
		page.Runs = append([]DocDisplayRun(nil), fragment.Runs...)
		return page, next, nil
	}
	if input.validate() != nil {
		return DocDisplayAssemblyInput{}, ErrIntegrity
	}
	return input, nil
}
