package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"unicode/utf16"
	"unicode/utf8"
)

func BuildMemoryReadBundle(ctx context.Context, header MemoryReadHeader, items []EffectiveMemoryItem) (MemoryReadBundle, error) {
	if err := memoryReadHeaderMetadata(ctx, header); err != nil {
		return MemoryReadBundle{}, err
	}
	if len(items) > MaxMemoryReadItems {
		return MemoryReadBundle{}, ErrMemoryProjectionLimit
	}
	if header.View.Total != len(items) {
		return MemoryReadBundle{}, ErrIntegrity
	}
	providedRefs := header.Chunks
	header.Chunks = nil
	header.Dependencies = slices.Clone(header.Dependencies)
	sort.Slice(header.Dependencies, func(left, right int) bool {
		return header.Dependencies[left].Hash < header.Dependencies[right].Hash
	})
	if err := memoryReadDependencies(ctx, header.Dependencies); err != nil {
		return MemoryReadBundle{}, err
	}
	var chunks []MemoryReadChunk
	body := []byte{'['}
	start, count, totalBytes := 0, 0, 0
	flush := func() error {
		if count == 0 {
			return nil
		}
		if len(chunks) == MaxMemoryReadChunks || len(body)+1 > MaxMemoryReadBytes-totalBytes {
			return ErrMemoryProjectionLimit
		}
		body = append(body, ']')
		digest := HashContent(body)
		header.Chunks = append(header.Chunks, MemoryReadChunkRef{Hash: digest, Start: start, Count: count, Bytes: len(body)})
		chunks = append(chunks, MemoryReadChunk{Hash: digest, Bytes: body})
		totalBytes += len(body)
		start += count
		body, count = []byte{'['}, 0
		return ctx.Err()
	}
	for _, item := range items {
		raw, err := memoryReadItemBytes(ctx, item)
		if err != nil {
			return MemoryReadBundle{}, err
		}
		separator := 0
		if count != 0 {
			separator = 1
		}
		if count == MaxMemoryReadChunkItems || len(body)+separator+len(raw)+1 > MaxMemoryReadChunkBytes {
			if err := flush(); err != nil {
				return MemoryReadBundle{}, err
			}
		}
		if count != 0 {
			body = append(body, ',')
		}
		body = append(body, raw...)
		count++
	}
	if err := flush(); err != nil {
		return MemoryReadBundle{}, err
	}
	if len(providedRefs) != 0 && !slices.Equal(providedRefs, header.Chunks) {
		return MemoryReadBundle{}, ErrIntegrity
	}
	rawHeader, err := MemoryReadHeaderBytes(ctx, header)
	if err != nil {
		return MemoryReadBundle{}, err
	}
	if err := memoryReadDelivery(ctx, header, items); err != nil {
		return MemoryReadBundle{}, err
	}
	var ownedHeader MemoryReadHeader
	if err := json.Unmarshal(rawHeader, &ownedHeader); err != nil {
		return MemoryReadBundle{}, ErrIntegrity
	}
	identifier := HashContent(rawHeader)
	if err := ctx.Err(); err != nil {
		return MemoryReadBundle{}, err
	}
	return MemoryReadBundle{ID: identifier, Header: ownedHeader, Chunks: chunks}, nil
}

func ValidateMemoryReadBundle(ctx context.Context, bundle MemoryReadBundle) error {
	rawHeader, err := MemoryReadHeaderBytes(ctx, bundle.Header)
	if err != nil {
		return err
	}
	if len(bundle.Chunks) > MaxMemoryReadChunks {
		return ErrMemoryProjectionLimit
	}
	if bundle.ID != HashContent(rawHeader) || len(bundle.Chunks) != len(bundle.Header.Chunks) {
		return ErrIntegrity
	}
	items := make([]EffectiveMemoryItem, 0, bundle.Header.View.Total)
	for ordinal, chunk := range bundle.Chunks {
		if chunk.Hash != bundle.Header.Chunks[ordinal].Hash {
			return ErrIntegrity
		}
		decoded, err := memoryReadDecodeChunk(ctx, bundle.Header.Chunks[ordinal], chunk.Bytes)
		if err != nil {
			return err
		}
		items = append(items, decoded...)
	}
	return memoryReadDelivery(ctx, bundle.Header, items)
}

func MemoryReadHeaderBytes(ctx context.Context, header MemoryReadHeader) ([]byte, error) {
	if err := memoryReadHeaderMetadata(ctx, header); err != nil {
		return nil, err
	}
	if err := memoryReadDependencies(ctx, header.Dependencies); err != nil {
		return nil, err
	}
	count, bodyBytes := 0, 0
	for _, reference := range header.Chunks {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if reference.Count > MaxMemoryReadChunkItems || reference.Bytes > MaxMemoryReadChunkBytes {
			return nil, ErrMemoryProjectionLimit
		}
		if reference.Start != count || reference.Count <= 0 || reference.Bytes < 2+reference.Count*3-1 ||
			ValidateContentHash(reference.Hash) != nil {
			return nil, ErrIntegrity
		}
		if reference.Count > MaxMemoryReadItems-count || reference.Bytes > MaxMemoryReadBytes-bodyBytes {
			return nil, ErrMemoryProjectionLimit
		}
		count += reference.Count
		bodyBytes += reference.Bytes
	}
	if count != header.View.Total {
		return nil, ErrIntegrity
	}
	raw, err := canonicalJSON(header)
	if err != nil {
		return nil, ErrIntegrity
	}
	if len(raw) > MaxMemoryReadHeaderBytes || len(raw) > MaxMemoryReadBytes-bodyBytes {
		return nil, ErrMemoryProjectionLimit
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return raw, nil
}

func DecodeMemoryReadChunk(ctx context.Context, header MemoryReadHeader, ordinal int, raw []byte) ([]EffectiveMemoryItem, error) {
	if _, err := MemoryReadHeaderBytes(ctx, header); err != nil {
		return nil, err
	}
	if ordinal < 0 || ordinal >= len(header.Chunks) {
		return nil, ErrIntegrity
	}
	return memoryReadDecodeChunk(ctx, header.Chunks[ordinal], raw)
}

func memoryReadHeaderMetadata(ctx context.Context, header MemoryReadHeader) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if header.View.Total > MaxMemoryReadItems || len(header.Chunks) > MaxMemoryReadChunks || len(header.Dependencies) > MaxMemoryCaptureObjects {
		return ErrMemoryProjectionLimit
	}
	if header.View.Items != nil || header.View.NextCursor != "" || header.View.Total < 0 {
		return ErrIntegrity
	}
	remaining := MaxMemoryReadHeaderBytes
	if err := memoryReadMeasure(ctx, reflect.ValueOf(header), &remaining); err != nil {
		return err
	}
	if header.Version != MemoryReadVersion || header.Rules != MemoryReadRules || header.Source.Validate() != nil ||
		header.View.Selection.Validate() != nil || ValidateContentHash(header.CaptureID) != nil ||
		ValidateContentHash(header.View.StateHash) != nil || ValidateContentHash(header.View.LineageHash) != nil ||
		ValidateContentHash(header.View.DeliveryStateHash) != nil {
		return ErrIntegrity
	}
	switch header.View.Content {
	case "", "claims", "prompt":
	default:
		return ErrIntegrity
	}
	return ctx.Err()
}

func memoryReadDependencies(ctx context.Context, dependencies []MemoryReadDependency) error {
	for index, dependency := range dependencies {
		if err := ctx.Err(); err != nil {
			return err
		}
		if ValidateContentHash(dependency.Hash) != nil || index > 0 && dependencies[index-1].Hash >= dependency.Hash {
			return ErrIntegrity
		}
	}
	return nil
}

func memoryReadDelivery(ctx context.Context, header MemoryReadHeader, items []EffectiveMemoryItem) error {
	digest, err := EffectiveMemoryDeliveryStateHash(ctx, header.Source.Graph.RepoID, header.Origin, header.View, items)
	if err != nil {
		return err
	}
	if digest != header.View.DeliveryStateHash {
		return ErrIntegrity
	}
	return ctx.Err()
}

func memoryReadItemBytes(ctx context.Context, item EffectiveMemoryItem) ([]byte, error) {
	remaining := MaxMemoryReadChunkBytes - 2
	if err := memoryReadMeasure(ctx, reflect.ValueOf(item), &remaining); err != nil {
		return nil, err
	}
	if ValidateContentHash(item.ID) != nil || ValidateContentHash(item.SourceSnapshot) != nil ||
		ValidateOptionalContentHash(item.TextHash) != nil || item.Part < 0 {
		return nil, ErrIntegrity
	}
	switch item.Kind {
	case "code":
		if item.Code == nil || ValidateGitOID(item.Code.Commit) != nil ||
			item.Code.Parent != "" && ValidateGitOID(item.Code.Parent) != nil || len(item.Code.Paths) == 0 {
			return nil, ErrIntegrity
		}
	case "decision", "rationale", "legacy_text", "legacy_summary", "legacy_fact", "legacy_task":
		if item.Code != nil {
			return nil, ErrIntegrity
		}
	default:
		return nil, ErrIntegrity
	}
	switch item.State {
	case "applied", "inactive", "retained", "review":
	default:
		return nil, ErrIntegrity
	}
	for _, path := range item.Paths {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if validateGitEntry(path.Before) != nil || validateGitEntry(path.After) != nil || validateGitEntry(path.Selected) != nil {
			return nil, ErrIntegrity
		}
		switch path.State {
		case "applied", "before", "changed", "equivalent", "not_in_history", "unknown":
		default:
			return nil, ErrIntegrity
		}
	}
	raw, err := json.Marshal(item)
	if err != nil {
		return nil, ErrIntegrity
	}
	if len(raw) > MaxMemoryReadChunkBytes-2 {
		return nil, ErrMemoryProjectionLimit
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return raw, nil
}

func memoryReadDecodeChunk(ctx context.Context, reference MemoryReadChunkRef, raw []byte) ([]EffectiveMemoryItem, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(raw) > MaxMemoryReadChunkBytes {
		return nil, ErrMemoryProjectionLimit
	}
	if len(raw) != reference.Bytes || HashContent(raw) != reference.Hash || !memoryReadJSONUnicode(raw) {
		return nil, ErrIntegrity
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if token, err := decoder.Token(); err != nil || token != json.Delim('[') {
		return nil, ErrIntegrity
	}
	items := make([]EffectiveMemoryItem, 0, reference.Count)
	for decoder.More() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if len(items) == reference.Count {
			return nil, ErrIntegrity
		}
		var itemRaw json.RawMessage
		if err := decoder.Decode(&itemRaw); err != nil {
			return nil, ErrIntegrity
		}
		if len(itemRaw) > MaxMemoryReadChunkBytes-2 {
			return nil, ErrMemoryProjectionLimit
		}
		itemDecoder := json.NewDecoder(bytes.NewReader(itemRaw))
		itemDecoder.UseNumber()
		if err := memoryReadJSONValue(ctx, itemDecoder, reflect.TypeFor[EffectiveMemoryItem]()); err != nil {
			return nil, err
		}
		var item EffectiveMemoryItem
		if err := json.Unmarshal(itemRaw, &item); err != nil {
			return nil, ErrIntegrity
		}
		if _, err := memoryReadItemBytes(ctx, item); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	if closing, err := decoder.Token(); err != nil || closing != json.Delim(']') || len(items) != reference.Count {
		return nil, ErrIntegrity
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, ErrIntegrity
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return items, nil
}

func memoryReadMeasure(ctx context.Context, value reflect.Value, remaining *int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	switch value.Kind() {
	case reflect.Pointer:
		if !value.IsNil() {
			return memoryReadMeasure(ctx, value.Elem(), remaining)
		}
		return memoryReadSize(4, remaining)
	case reflect.String:
		text := value.String()
		if err := memoryReadSize(2, remaining); err != nil {
			return err
		}
		if err := memoryReadSize(len(text), remaining); err != nil {
			return err
		}
		for offset := 0; offset < len(text); {
			if err := ctx.Err(); err != nil {
				return err
			}
			end := min(offset+4096, len(text))
			for end < len(text) && !utf8.RuneStart(text[end]) {
				end++
			}
			if !utf8.ValidString(text[offset:end]) {
				return ErrIntegrity
			}
			offset = end
		}
	case reflect.Slice:
		if value.IsNil() {
			return memoryReadSize(4, remaining)
		}
		if err := memoryReadSize(2+max(0, value.Len()-1), remaining); err != nil {
			return err
		}
		for index := 0; index < value.Len(); index++ {
			if err := memoryReadMeasure(ctx, value.Index(index), remaining); err != nil {
				return err
			}
		}
	case reflect.Struct:
		if err := memoryReadSize(2, remaining); err != nil {
			return err
		}
		compiled, err := graphProjectionJSONStruct(value.Type())
		if err != nil {
			return err
		}
		separator := 0
		for name, field := range compiled.fields {
			nested := value.FieldByIndex(field.definition.Index)
			if !field.required && (nested.IsZero() || nested.Kind() == reflect.Slice && nested.Len() == 0) {
				continue
			}
			if err := memoryReadSize(len(name)+3+separator, remaining); err != nil {
				return err
			}
			separator = 1
			if field.quoted {
				if err := memoryReadSize(2, remaining); err != nil {
					return err
				}
			}
			if err := memoryReadMeasure(ctx, nested, remaining); err != nil {
				return err
			}
		}
	case reflect.Bool:
		return memoryReadSize(len(strconv.FormatBool(value.Bool())), remaining)
	case reflect.Int, reflect.Int64:
		return memoryReadSize(len(strconv.FormatInt(value.Int(), 10)), remaining)
	case reflect.Uint64:
		return memoryReadSize(len(strconv.FormatUint(value.Uint(), 10)), remaining)
	default:
		return ErrIntegrity
	}
	return nil
}

func memoryReadSize(count int, remaining *int) error {
	if count > *remaining {
		return ErrMemoryProjectionLimit
	}
	*remaining -= count
	return nil
}

func memoryReadJSONValue(ctx context.Context, decoder *json.Decoder, schema reflect.Type) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	token, err := decoder.Token()
	if err != nil {
		return ErrIntegrity
	}
	if token == nil {
		if schema.Kind() == reflect.Slice || schema.Kind() == reflect.Pointer {
			return nil
		}
		return ErrIntegrity
	}
	for schema.Kind() == reflect.Pointer {
		schema = schema.Elem()
	}
	switch schema.Kind() {
	case reflect.String:
		if _, valid := token.(string); !valid {
			return ErrIntegrity
		}
	case reflect.Int:
		if _, valid := token.(json.Number); !valid {
			return ErrIntegrity
		}
	case reflect.Slice:
		if token != json.Delim('[') {
			return ErrIntegrity
		}
		for decoder.More() {
			if err := memoryReadJSONValue(ctx, decoder, schema.Elem()); err != nil {
				return err
			}
		}
		if closing, err := decoder.Token(); err != nil || closing != json.Delim(']') {
			return ErrIntegrity
		}
	case reflect.Struct:
		if token != json.Delim('{') {
			return ErrIntegrity
		}
		compiled, err := graphProjectionJSONStruct(schema)
		if err != nil {
			return err
		}
		seen := make([]bool, len(compiled.fields))
		required := compiled.required
		for decoder.More() {
			keyToken, err := decoder.Token()
			key, valid := keyToken.(string)
			if err != nil || !valid {
				return ErrIntegrity
			}
			field, exists := compiled.fields[key]
			if !exists || seen[field.position] {
				return ErrIntegrity
			}
			seen[field.position] = true
			if field.required {
				required--
			}
			if err := memoryReadJSONValue(ctx, decoder, field.definition.Type); err != nil {
				return err
			}
		}
		if closing, err := decoder.Token(); err != nil || closing != json.Delim('}') || required != 0 {
			return ErrIntegrity
		}
	default:
		return ErrIntegrity
	}
	return nil
}

func memoryReadJSONUnicode(raw []byte) bool {
	if !utf8.Valid(raw) {
		return false
	}
	for offset := 0; offset < len(raw); offset++ {
		if raw[offset] != '\\' {
			continue
		}
		offset++
		if offset >= len(raw) || raw[offset] != 'u' {
			continue
		}
		if offset+5 > len(raw) {
			return false
		}
		code, err := strconv.ParseUint(string(raw[offset+1:offset+5]), 16, 16)
		if err != nil {
			return false
		}
		offset += 4
		if !utf16.IsSurrogate(rune(code)) {
			continue
		}
		if code >= 0xdc00 || offset+7 > len(raw) || raw[offset+1] != '\\' || raw[offset+2] != 'u' {
			return false
		}
		low, err := strconv.ParseUint(string(raw[offset+3:offset+7]), 16, 16)
		if err != nil || low < 0xdc00 || low > 0xdfff {
			return false
		}
		offset += 6
	}
	return true
}
