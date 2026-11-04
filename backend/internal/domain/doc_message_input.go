package domain

import (
	"bytes"
	"context"
	"encoding/json"
)

const DocMessageInputProfile = "public-message-input-v1"
const DocMessageInputPageEntries = 64
const MaxDocMessageInputPageBytes = 32 << 10
const DocMessageInputStepEntries = 128

type DocMessageInputSource struct {
	Version    int         `json:"version"`
	Profile    string      `json:"profile"`
	EventHash  ContentHash `json:"event_hash"`
	EventBytes int         `json:"event_bytes"`
	Blocks     DocFindSpan `json:"blocks"`
	Summary    bool        `json:"summary"`
	Empty      bool        `json:"empty,omitempty"`
}

func (source DocMessageInputSource) Validate() error {
	if source.Version != 1 || source.Profile != DocMessageInputProfile || ValidateContentHash(source.EventHash) != nil || source.EventBytes <= 0 || source.EventBytes > MaxConversationDocumentBytes {
		return ErrIntegrity
	}
	if source.Empty {
		if source.Blocks != (DocFindSpan{}) {
			return ErrIntegrity
		}
	} else if source.Blocks.From < 0 || source.Blocks.From > source.EventBytes-2 || source.Blocks.To <= source.Blocks.From || source.Blocks.To > source.EventBytes || source.Blocks.To-source.Blocks.From < 2 {
		return ErrIntegrity
	}
	return nil
}

func (source DocMessageInputSource) Encode() ([]byte, error) {
	if source.Validate() != nil {
		return nil, ErrIntegrity
	}
	return canonicalJSON(source)
}

func (source DocMessageInputSource) Hash() (ContentHash, error) {
	raw, err := source.Encode()
	if err != nil {
		return "", err
	}
	return HashContent(raw), nil
}

func DecodeDocMessageInputSource(expected ContentHash, raw []byte) (DocMessageInputSource, error) {
	var source DocMessageInputSource
	if len(raw) == 0 || len(raw) > MaxDocDisplaySourceBytes || ValidateContentHash(expected) != nil || HashContent(raw) != expected || json.Unmarshal(raw, &source) != nil {
		return DocMessageInputSource{}, ErrIntegrity
	}
	canonical, err := source.Encode()
	if err != nil || !bytes.Equal(raw, canonical) {
		return DocMessageInputSource{}, ErrIntegrity
	}
	return source, nil
}

func (state DocFieldDiscoveryState) MessageInputSource(entry DocRangeEntry) (DocMessageInputSource, error) {
	if state.Validate() != nil || !state.Done {
		return DocMessageInputSource{}, ErrIntegrity
	}
	if state.Kind != EventMessage {
		return DocMessageInputSource{}, ErrDocFindDisplayUnavailable
	}
	if entry.Hash != state.EventHash || entry.Length != state.EventBytes || entry.Kind != EventMessage {
		return DocMessageInputSource{}, ErrIntegrity
	}
	fields, err := state.FindFields()
	if err != nil {
		return DocMessageInputSource{}, err
	}
	source := DocMessageInputSource{Version: 1, Profile: DocMessageInputProfile, EventHash: state.EventHash, EventBytes: state.EventBytes, Summary: entry.CompactSummary || entry.SeedSummary, Empty: fields.Blocks == nil}
	if fields.Blocks != nil {
		source.Blocks = *fields.Blocks
	}
	return source, source.Validate()
}

func (source DocMessageInputSource) maximumEntries() int {
	if source.Empty {
		return 0
	}
	return (source.Blocks.To - source.Blocks.From - 1) / 3
}

type DocMessageBlockInput struct {
	Index  int          `json:"index"`
	Anchor DocFindSpan  `json:"anchor"`
	Type   *DocFindSpan `json:"type,omitempty"`
	Name   *DocFindSpan `json:"name,omitempty"`
	Text   *DocFindSpan `json:"text,omitempty"`
	IsText bool         `json:"is_text"`
}

func (entry DocMessageBlockInput) Validate(source DocMessageInputSource) error {
	if source.Validate() != nil || entry.Index < 0 || entry.Index >= source.maximumEntries() || entry.Anchor.From <= source.Blocks.From || entry.Anchor.To >= source.Blocks.To || entry.Anchor.To <= entry.Anchor.From || entry.Anchor.To-entry.Anchor.From < 2 {
		return ErrIntegrity
	}
	if int64(entry.Anchor.From-source.Blocks.From-1) < int64(entry.Index)*3 {
		return ErrIntegrity
	}
	spans := []*DocFindSpan{entry.Type, entry.Name, entry.Text}
	for index, span := range spans {
		if span != nil && (span.From <= entry.Anchor.From || span.To >= entry.Anchor.To || span.To <= span.From || span.To-span.From < 2) {
			return ErrIntegrity
		}
		for _, previous := range spans[:index] {
			if span != nil && previous != nil && span.From < previous.To && previous.From < span.To {
				return ErrIntegrity
			}
		}
	}
	if entry.IsText && (entry.Type == nil || entry.Type.To-entry.Type.From < 6) {
		return ErrIntegrity
	}
	return nil
}

type DocMessageInputPage struct {
	Version int                    `json:"version"`
	Source  ContentHash            `json:"source"`
	Index   int                    `json:"index"`
	Entries []DocMessageBlockInput `json:"entries"`
}

func (page DocMessageInputPage) Validate(source DocMessageInputSource) error {
	hash, err := source.Hash()
	if err != nil || page.Version != 1 || page.Source != hash || page.Index < 0 || page.Index >= MaxDocRangePages || page.Index > source.maximumEntries()/DocMessageInputPageEntries || len(page.Entries) == 0 || len(page.Entries) > DocMessageInputPageEntries {
		return ErrIntegrity
	}
	previous := source.Blocks.From
	for index, entry := range page.Entries {
		if entry.Validate(source) != nil || entry.Index != page.Index*DocMessageInputPageEntries+index || entry.Anchor.From <= previous {
			return ErrIntegrity
		}
		previous = entry.Anchor.To
	}
	return nil
}

func (page DocMessageInputPage) Encode(source DocMessageInputSource) ([]byte, error) {
	if page.Validate(source) != nil {
		return nil, ErrIntegrity
	}
	raw, err := marshalDocMessageInputPage(page)
	if err != nil || len(raw) > MaxDocMessageInputPageBytes {
		return nil, ErrIntegrity
	}
	return raw, nil
}

func DecodeDocMessageInputPage(source DocMessageInputSource, expected ContentHash, raw []byte) (DocMessageInputPage, error) {
	var page DocMessageInputPage
	if len(raw) == 0 || len(raw) > MaxDocMessageInputPageBytes || ValidateContentHash(expected) != nil || HashContent(raw) != expected || json.Unmarshal(raw, &page) != nil {
		return DocMessageInputPage{}, ErrIntegrity
	}
	canonical, err := page.Encode(source)
	if err != nil || !bytes.Equal(raw, canonical) {
		return DocMessageInputPage{}, ErrIntegrity
	}
	return page, nil
}

type DocMessageInputGeneration struct {
	Version int                   `json:"version"`
	Source  DocMessageInputSource `json:"source"`
	Count   int                   `json:"count"`
	Pages   DocRangeDirectoryRoot `json:"pages"`
}

func (generation DocMessageInputGeneration) Validate() error {
	if generation.Version != 1 || generation.Source.Validate() != nil || generation.Count < 0 || generation.Count > generation.Source.maximumEntries() || generation.Pages.validate((generation.Count+DocMessageInputPageEntries-1)/DocMessageInputPageEntries) != nil {
		return ErrIntegrity
	}
	return nil
}

func (generation DocMessageInputGeneration) Encode() ([]byte, error) {
	if generation.Validate() != nil {
		return nil, ErrIntegrity
	}
	return canonicalJSON(generation)
}

func DecodeDocMessageInputGeneration(expected ContentHash, raw []byte) (DocMessageInputGeneration, error) {
	var generation DocMessageInputGeneration
	if len(raw) == 0 || len(raw) > MaxDocDisplaySourceBytes || ValidateContentHash(expected) != nil || HashContent(raw) != expected || json.Unmarshal(raw, &generation) != nil {
		return DocMessageInputGeneration{}, ErrIntegrity
	}
	canonical, err := generation.Encode()
	if err != nil || !bytes.Equal(raw, canonical) {
		return DocMessageInputGeneration{}, ErrIntegrity
	}
	return generation, nil
}

type DocMessageInput struct {
	source DocMessageInputSource
	entry  DocMessageBlockInput
}

func (input DocMessageInput) Index() int          { return input.entry.Index }
func (input DocMessageInput) Anchor() DocFindSpan { return input.entry.Anchor }
func (input DocMessageInput) SummarySlot() bool   { return input.source.Summary }
func (input DocMessageInput) IsText() bool        { return input.entry.IsText }

func (input DocMessageInput) decodedSource(span *DocFindSpan) (*DocDecodedTextSource, error) {
	if input.source.Validate() != nil || input.entry.Validate(input.source) != nil {
		return nil, ErrIntegrity
	}
	if span == nil {
		return nil, nil
	}
	source := DocDecodedTextSource{Version: 1, Profile: DocDecodedTextProfile, EventHash: input.source.EventHash, EventBytes: input.source.EventBytes, Token: *span}
	if err := source.Validate(); err != nil {
		return nil, err
	}
	return &source, nil
}

func (input DocMessageInput) TextSource() (*DocDecodedTextSource, error) {
	if !input.source.Summary && !input.entry.IsText {
		return nil, ErrDocFindDisplayUnavailable
	}
	return input.decodedSource(input.entry.Text)
}

func (input DocMessageInput) PlaceholderSources() (blockType, name *DocDecodedTextSource, failure error) {
	if input.source.Summary || input.entry.IsText {
		return nil, nil, ErrDocFindDisplayUnavailable
	}
	blockType, err := input.decodedSource(input.entry.Type)
	if err != nil {
		return nil, nil, err
	}
	name, err = input.decodedSource(input.entry.Name)
	return blockType, name, err
}

type docMessageInputReader struct {
	generation       DocMessageInputGeneration
	cachedGeneration DocMessageInputGeneration
	index            int
	hash             ContentHash
	inputs           []DocMessageInput
}

func ReadDocMessageInputs(ctx context.Context, generation DocMessageInputGeneration, index int, load func(context.Context, ContentHash) ([]byte, error)) ([]DocMessageInput, error) {
	reader := docMessageInputReader{generation: generation}
	return reader.read(ctx, index, load)
}

func (reader *docMessageInputReader) read(ctx context.Context, index int, load func(context.Context, ContentHash) ([]byte, error)) (result []DocMessageInput, failure error) {
	defer func() {
		if err := ctx.Err(); err != nil {
			result, failure = nil, err
		}
	}()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if reader == nil || reader.generation.Validate() != nil || index < 0 || index >= reader.generation.Pages.Count || load == nil {
		return nil, ErrIntegrity
	}
	generation := reader.generation
	hash, err := ResolveDocRangeLeaf(ctx, generation.Pages, index, load)
	if err != nil {
		return nil, err
	}
	raw, err := load(ctx, hash)
	if err != nil {
		return nil, err
	}
	if reader.cachedGeneration == generation && reader.index == index && reader.hash == hash && len(reader.inputs) > 0 {
		if len(raw) == 0 || len(raw) > MaxDocMessageInputPageBytes || ValidateContentHash(hash) != nil || HashContent(raw) != hash {
			return nil, ErrIntegrity
		}
		return reader.inputs, nil
	}
	page, err := DecodeDocMessageInputPage(generation.Source, hash, raw)
	if err != nil || page.Index != index || len(page.Entries) != min(DocMessageInputPageEntries, generation.Count-index*DocMessageInputPageEntries) {
		return nil, ErrIntegrity
	}
	last := page.Entries[len(page.Entries)-1]
	if int64(generation.Source.Blocks.To-last.Anchor.To-1) < int64(generation.Count-last.Index-1)*3 {
		return nil, ErrIntegrity
	}
	for _, entry := range page.Entries {
		result = append(result, DocMessageInput{source: generation.Source, entry: entry})
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	reader.cachedGeneration, reader.index, reader.hash, reader.inputs = generation, index, hash, result
	return result, nil
}
