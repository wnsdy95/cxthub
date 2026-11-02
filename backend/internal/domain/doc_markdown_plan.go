package domain

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding"
	"encoding/json"
)

const DocMarkdownPlanOperations = 128
const MaxDocMarkdownPlanPageBytes = 32 << 10
const DocMarkdownPlanStepOperations = 128
const DocMarkdownPlanStepActions = 1 << 16

type DocMarkdownPlanInput struct {
	Hash  ContentHash `json:"hash"`
	Bytes int         `json:"bytes"`
}

func (input DocMarkdownPlanInput) Validate() error {
	if ValidateContentHash(input.Hash) != nil || input.Bytes < 0 || input.Bytes > MaxConversationDocumentBytes || input.Bytes == 0 && input.Hash != HashContent(nil) {
		return ErrIntegrity
	}
	return nil
}

func (input DocMarkdownPlanInput) identity() ContentHash {
	raw, _ := canonicalJSON(input)
	return HashContent(raw)
}

type DocMarkdownPlanOperation struct {
	From int    `json:"from"`
	To   int    `json:"to"`
	Text string `json:"text,omitempty"`
}

func (operation DocMarkdownPlanOperation) Validate(input DocMarkdownPlanInput) error {
	if operation.From < 0 || operation.To <= operation.From || operation.To > input.Bytes || operation.Text != "" && operation.Text != " " && operation.Text != "\n" && operation.Text != "\n\n" {
		return ErrIntegrity
	}
	return nil
}

func (operation DocMarkdownPlanOperation) TextBytes() int {
	if operation.Text != "" {
		return len(operation.Text)
	}
	return operation.To - operation.From
}

func (operation DocMarkdownPlanOperation) linearCoordinates() bool {
	return operation.To-operation.From == operation.TextBytes()
}

type DocMarkdownPlanPage struct {
	Version    int                        `json:"version"`
	Input      ContentHash                `json:"input"`
	Index      int                        `json:"index"`
	From       int                        `json:"from"`
	Operations []DocMarkdownPlanOperation `json:"operations"`
}

func (page DocMarkdownPlanPage) Validate(input DocMarkdownPlanInput) error {
	if input.Validate() != nil || page.Version != 1 || page.Input != input.identity() || page.Index < 0 || page.Index >= MaxDocRangePages || page.From < 0 || page.From >= MaxConversationDocumentBytes || page.Index > page.From/DocMarkdownPlanOperations || page.Index == 0 && page.From != 0 || len(page.Operations) == 0 || len(page.Operations) > DocMarkdownPlanOperations {
		return ErrIntegrity
	}
	total := page.From
	for _, operation := range page.Operations {
		if operation.Validate(input) != nil || operation.TextBytes() > MaxConversationDocumentBytes-total {
			return ErrIntegrity
		}
		total += operation.TextBytes()
	}
	return nil
}

func (page DocMarkdownPlanPage) Encode(input DocMarkdownPlanInput) ([]byte, error) {
	if page.Validate(input) != nil {
		return nil, ErrIntegrity
	}
	raw, err := canonicalJSON(page)
	if err != nil || len(raw) > MaxDocMarkdownPlanPageBytes {
		return nil, ErrIntegrity
	}
	return raw, nil
}

func DecodeDocMarkdownPlanPage(input DocMarkdownPlanInput, expected ContentHash, raw []byte) (DocMarkdownPlanPage, error) {
	var page DocMarkdownPlanPage
	if len(raw) == 0 || len(raw) > MaxDocMarkdownPlanPageBytes || ValidateContentHash(expected) != nil || HashContent(raw) != expected || json.Unmarshal(raw, &page) != nil {
		return DocMarkdownPlanPage{}, ErrIntegrity
	}
	canonical, err := page.Encode(input)
	if err != nil || !bytes.Equal(canonical, raw) {
		return DocMarkdownPlanPage{}, ErrIntegrity
	}
	return page, nil
}

const (
	docMarkdownPlanBlockStart = iota + 1
	docMarkdownPlanParagraph
	docMarkdownPlanFence
	docMarkdownPlanInline
	docMarkdownPlanCopy
	docMarkdownPlanBlockEnd
	docMarkdownPlanFinish
	docMarkdownPlanDone
)

type docMarkdownPlanBlock struct {
	From     int  `json:"from"`
	To       int  `json:"to"`
	Breaks   int  `json:"breaks"`
	Collapse bool `json:"collapse"`
}

type DocMarkdownPlanState struct {
	Version          int                          `json:"version"`
	Input            DocMarkdownPlanInput         `json:"input"`
	Phase            int                          `json:"phase"`
	Line             docMarkdownLineScan          `json:"line"`
	LineDone         bool                         `json:"line_done"`
	PendingLine      *docMarkdownPlanLine         `json:"pending_line,omitempty"`
	Scanned          int                          `json:"scanned"`
	HashNext         int                          `json:"hash_next"`
	HashState        []byte                       `json:"hash_state,omitempty"`
	Digest           ContentHash                  `json:"digest,omitempty"`
	Block            docMarkdownPlanBlock         `json:"block"`
	InlineCursor     int                          `json:"inline_cursor"`
	NextDelimiter    [4]int                       `json:"next_delimiter"`
	LookupKind       int                          `json:"lookup_kind"`
	LookupStart      int                          `json:"lookup_start"`
	LookupNext       int                          `json:"lookup_next"`
	WriteFrom        int                          `json:"write_from"`
	WriteTo          int                          `json:"write_to"`
	ReturnInline     bool                         `json:"return_inline"`
	CopyFrom         int                          `json:"copy_from"`
	CopyTo           int                          `json:"copy_to"`
	PendingSpaceFrom int                          `json:"pending_space_from"`
	PendingSpaceTo   int                          `json:"pending_space_to"`
	BlockStarted     bool                         `json:"block_started"`
	LastSourceEnd    int                          `json:"last_source_end"`
	PendingBreaks    int                          `json:"pending_breaks"`
	LeadingBreaks    int                          `json:"leading_breaks"`
	TextBytes        int                          `json:"text_bytes"`
	OperationCount   int                          `json:"operation_count"`
	PageFrom         int                          `json:"page_from"`
	Operations       []DocMarkdownPlanOperation   `json:"operations,omitempty"`
	Directory        DocRangeDirectoryAccumulator `json:"directory"`
}

func NewDocMarkdownPlanState(input DocMarkdownPlanInput) (DocMarkdownPlanState, error) {
	if input.Validate() != nil {
		return DocMarkdownPlanState{}, ErrIntegrity
	}
	hashState, err := sha256.New().(encoding.BinaryMarshaler).MarshalBinary()
	if err != nil {
		return DocMarkdownPlanState{}, err
	}
	return DocMarkdownPlanState{Version: 1, Input: input, Phase: docMarkdownPlanBlockStart, Line: newDocMarkdownLineScan(0), HashState: hashState, NextDelimiter: [4]int{-1, -1, -1, -1}, CopyFrom: -1, PendingSpaceFrom: -1}, nil
}

func (state DocMarkdownPlanState) Done() bool { return state.Phase == docMarkdownPlanDone }

func (state DocMarkdownPlanState) Validate() error {
	if state.Version != 1 || state.Input.Validate() != nil || state.Phase < docMarkdownPlanBlockStart || state.Phase > docMarkdownPlanDone || state.Line.validate(state.Input.Bytes) != nil || state.Scanned < 0 || state.Scanned > state.Input.Bytes || state.HashNext < 0 || state.HashNext > state.Scanned || state.Scanned-state.HashNext > sha256.BlockSize+4 || state.TextBytes < 0 || state.TextBytes > MaxConversationDocumentBytes || state.PageFrom < 0 || state.PageFrom > state.TextBytes || state.OperationCount < 0 || state.OperationCount > state.TextBytes || len(state.Operations) >= DocMarkdownPlanOperations || state.Directory.Validate() != nil || state.PendingBreaks < 0 || state.PendingBreaks > 2 || state.LeadingBreaks < 0 || state.LeadingBreaks > 2 || state.LastSourceEnd < 0 || state.LastSourceEnd > state.Input.Bytes {
		return ErrIntegrity
	}
	if state.Digest == "" {
		if !validDocFieldHashState(state.HashState, state.HashNext) {
			return ErrIntegrity
		}
	} else if state.Digest != state.Input.Hash || state.Scanned != state.Input.Bytes || state.HashNext != state.Input.Bytes || len(state.HashState) != 0 {
		return ErrIntegrity
	}
	if state.Line.Next != state.Scanned || state.LineDone && (state.Scanned != state.Input.Bytes || state.Digest != state.Input.Hash) || state.LastSourceEnd > state.Scanned {
		return ErrIntegrity
	}
	if (state.Directory.Count == 0) != (state.PageFrom == 0) || state.TextBytes > 0 && state.LeadingBreaks == 0 || state.Directory.Count > 0 && state.Phase != docMarkdownPlanDone && state.Directory.Count > state.PageFrom/DocMarkdownPlanOperations {
		return ErrIntegrity
	}
	if state.Block.From < 0 || state.Block.To < state.Block.From || state.Block.To > state.Input.Bytes || state.Block.Breaks < 0 || state.Block.Breaks > 2 || state.InlineCursor < 0 || state.InlineCursor > state.Input.Bytes || state.WriteFrom < 0 || state.WriteTo < state.WriteFrom || state.WriteTo > state.Input.Bytes || state.LookupKind < 0 || state.LookupKind > 4 || state.LookupStart < 0 || state.LookupStart > state.Input.Bytes || state.LookupNext < state.LookupStart || state.LookupNext > state.Input.Bytes {
		return ErrIntegrity
	}
	for _, position := range state.NextDelimiter {
		if position < -1 || position > state.Input.Bytes {
			return ErrIntegrity
		}
		if (state.Phase == docMarkdownPlanInline || state.Phase == docMarkdownPlanCopy && state.ReturnInline) && position >= 0 && (position < state.Block.From || position > state.Block.To) {
			return ErrIntegrity
		}
	}
	if state.LookupKind > 0 && (state.LookupStart < state.Block.From || state.LookupNext > state.Block.To) {
		return ErrIntegrity
	}
	if state.Phase >= docMarkdownPlanParagraph && state.Phase <= docMarkdownPlanBlockEnd && state.Block.Breaks == 0 || state.Phase == docMarkdownPlanInline && (state.InlineCursor < state.Block.From || state.InlineCursor > state.Block.To) || state.Phase == docMarkdownPlanCopy && (state.WriteFrom < state.Block.From || state.WriteTo > state.Block.To) || state.LookupKind > 0 && state.Phase != docMarkdownPlanInline {
		return ErrIntegrity
	}
	if state.CopyFrom < -1 || state.CopyFrom > state.Input.Bytes || state.CopyTo < 0 || state.CopyTo > state.Input.Bytes || state.CopyFrom >= 0 && state.CopyFrom >= state.CopyTo || state.CopyFrom < 0 && state.CopyTo != 0 || state.PendingSpaceFrom < -1 || state.PendingSpaceFrom > state.Input.Bytes || state.PendingSpaceTo < 0 || state.PendingSpaceTo > state.Input.Bytes || state.PendingSpaceFrom >= 0 && state.PendingSpaceFrom >= state.PendingSpaceTo || state.PendingSpaceFrom < 0 && state.PendingSpaceTo != 0 {
		return ErrIntegrity
	}
	if state.PendingLine != nil && (state.PendingLine.From < 0 || state.PendingLine.To < state.PendingLine.From || state.PendingLine.To > state.Input.Bytes || state.PendingLine.Next != state.PendingLine.To+1 || state.PendingLine.ContentFrom < state.PendingLine.From || state.PendingLine.ContentFrom > state.PendingLine.To || state.PendingLine.Kind > docMarkdownLineList) {
		return ErrIntegrity
	}
	if state.PendingLine != nil && min(state.PendingLine.Next, state.Input.Bytes) != state.Scanned {
		return ErrIntegrity
	}
	total := state.PageFrom
	for _, operation := range state.Operations {
		if operation.Validate(state.Input) != nil || operation.TextBytes() > MaxConversationDocumentBytes-total {
			return ErrIntegrity
		}
		total += operation.TextBytes()
	}
	if state.CopyFrom >= 0 {
		if state.CopyTo-state.CopyFrom > MaxConversationDocumentBytes-total {
			return ErrIntegrity
		}
		total += state.CopyTo - state.CopyFrom
	}
	if total != state.TextBytes {
		return ErrIntegrity
	}
	if state.Phase == docMarkdownPlanDone {
		if state.Digest != state.Input.Hash || !state.LineDone || state.PendingLine != nil || state.CopyFrom >= 0 || len(state.Operations) != 0 || state.PageFrom != state.TextBytes || state.Directory.Count != (state.OperationCount+DocMarkdownPlanOperations-1)/DocMarkdownPlanOperations {
			return ErrIntegrity
		}
	} else if state.OperationCount != state.Directory.Count*DocMarkdownPlanOperations+len(state.Operations) {
		return ErrIntegrity
	}
	if state.Scanned == 0 && state.Input.Bytes > 0 {
		initial, err := NewDocMarkdownPlanState(state.Input)
		if err != nil {
			return err
		}
		actual, _ := canonicalJSON(state)
		expected, _ := canonicalJSON(initial)
		if !bytes.Equal(actual, expected) {
			return ErrIntegrity
		}
	}
	return nil
}

func (state DocMarkdownPlanState) Encode() ([]byte, error) {
	if state.Validate() != nil {
		return nil, ErrIntegrity
	}
	raw, err := canonicalJSON(state)
	if err != nil || len(raw) > MaxDocDisplayStateBytes {
		return nil, ErrIntegrity
	}
	return raw, nil
}

func DecodeDocMarkdownPlanState(expected ContentHash, raw []byte) (DocMarkdownPlanState, error) {
	var state DocMarkdownPlanState
	if len(raw) == 0 || len(raw) > MaxDocDisplayStateBytes || ValidateContentHash(expected) != nil || HashContent(raw) != expected || json.Unmarshal(raw, &state) != nil {
		return DocMarkdownPlanState{}, ErrIntegrity
	}
	canonical, err := state.Encode()
	if err != nil || !bytes.Equal(canonical, raw) {
		return DocMarkdownPlanState{}, ErrIntegrity
	}
	return state, nil
}

type DocMarkdownPlan struct {
	Version        int                   `json:"version"`
	Input          DocMarkdownPlanInput  `json:"input"`
	TextBytes      int                   `json:"text_bytes"`
	OperationCount int                   `json:"operation_count"`
	Pages          DocRangeDirectoryRoot `json:"pages"`
	LeadingBreaks  int                   `json:"leading_breaks"`
	TrailingBreaks int                   `json:"trailing_breaks"`
}

func (plan DocMarkdownPlan) Validate() error {
	if plan.Version != 1 || plan.Input.Validate() != nil || plan.TextBytes < 0 || plan.TextBytes > MaxConversationDocumentBytes || plan.OperationCount < 0 || plan.OperationCount > plan.TextBytes || (plan.OperationCount == 0) != (plan.TextBytes == 0) || plan.Pages.validate((plan.OperationCount+DocMarkdownPlanOperations-1)/DocMarkdownPlanOperations) != nil || plan.LeadingBreaks < 0 || plan.LeadingBreaks > 2 || plan.TrailingBreaks < 0 || plan.TrailingBreaks > 2 {
		return ErrIntegrity
	}
	if plan.TextBytes == 0 && plan.LeadingBreaks != 0 || plan.TextBytes > 0 && (plan.LeadingBreaks == 0 || plan.TrailingBreaks == 0) || plan.Input.Bytes == 0 && (plan.TextBytes != 0 || plan.TrailingBreaks != 0) {
		return ErrIntegrity
	}
	return nil
}

func (plan DocMarkdownPlan) Encode() ([]byte, error) {
	if plan.Validate() != nil {
		return nil, ErrIntegrity
	}
	return canonicalJSON(plan)
}

func DecodeDocMarkdownPlan(expected ContentHash, raw []byte) (DocMarkdownPlan, error) {
	var plan DocMarkdownPlan
	if len(raw) == 0 || len(raw) > MaxDocDisplaySourceBytes || ValidateContentHash(expected) != nil || HashContent(raw) != expected || json.Unmarshal(raw, &plan) != nil {
		return DocMarkdownPlan{}, ErrIntegrity
	}
	canonical, err := plan.Encode()
	if err != nil || !bytes.Equal(canonical, raw) {
		return DocMarkdownPlan{}, ErrIntegrity
	}
	return plan, nil
}

func FinishDocMarkdownPlan(ctx context.Context, state DocMarkdownPlanState) (result DocMarkdownPlan, objects []DocRangeObject, failure error) {
	defer func() {
		if err := ctx.Err(); err != nil {
			result, objects, failure = DocMarkdownPlan{}, nil, err
		}
	}()
	if err := ctx.Err(); err != nil {
		return DocMarkdownPlan{}, nil, err
	}
	if state.Validate() != nil || state.Phase != docMarkdownPlanDone {
		return DocMarkdownPlan{}, nil, ErrIntegrity
	}
	root, objects, err := state.Directory.Finish(ctx)
	if err != nil {
		return DocMarkdownPlan{}, nil, err
	}
	plan := DocMarkdownPlan{Version: 1, Input: state.Input, TextBytes: state.TextBytes, OperationCount: state.OperationCount, Pages: root, LeadingBreaks: state.LeadingBreaks, TrailingBreaks: state.PendingBreaks}
	if plan.Validate() != nil {
		return DocMarkdownPlan{}, nil, ErrIntegrity
	}
	return plan, objects, nil
}

func ReadDocMarkdownPlanPage(ctx context.Context, plan DocMarkdownPlan, index int, load func(context.Context, ContentHash) ([]byte, error)) (result DocMarkdownPlanPage, failure error) {
	defer func() {
		if err := ctx.Err(); err != nil {
			result, failure = DocMarkdownPlanPage{}, err
		}
	}()
	if err := ctx.Err(); err != nil {
		return DocMarkdownPlanPage{}, err
	}
	if plan.Validate() != nil || index < 0 || index >= plan.Pages.Count || load == nil {
		return DocMarkdownPlanPage{}, ErrIntegrity
	}
	hash, err := ResolveDocRangeLeaf(ctx, plan.Pages, index, load)
	if err != nil {
		return DocMarkdownPlanPage{}, err
	}
	raw, err := load(ctx, hash)
	if err != nil {
		return DocMarkdownPlanPage{}, err
	}
	page, err := DecodeDocMarkdownPlanPage(plan.Input, hash, raw)
	if err != nil || page.Index != index || len(page.Operations) != min(DocMarkdownPlanOperations, plan.OperationCount-index*DocMarkdownPlanOperations) || page.From > plan.TextBytes {
		return DocMarkdownPlanPage{}, ErrIntegrity
	}
	end := page.From
	for _, operation := range page.Operations {
		if operation.TextBytes() > plan.TextBytes-end {
			return DocMarkdownPlanPage{}, ErrIntegrity
		}
		end += operation.TextBytes()
	}
	remaining := plan.OperationCount - index*DocMarkdownPlanOperations - len(page.Operations)
	if remaining > plan.TextBytes-end || (remaining == 0) != (end == plan.TextBytes) {
		return DocMarkdownPlanPage{}, ErrIntegrity
	}
	return page, nil
}
