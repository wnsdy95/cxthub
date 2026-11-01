package domain

import (
	"bytes"
	"context"
	"encoding/json"
)

const docToolEventProfile = "public-tool-event-plan-v1"
const docToolEventParts = 16

var docToolPolicyKeys = [...]string{"file_path", "command", "pattern", "url", "old_string", "new_string", "content"}

type docToolPolicySource struct {
	Version   int                   `json:"version"`
	Event     DocEventDisplaySource `json:"event"`
	Fields    ContentHash           `json:"fields"`
	Tree      *DocToolTreeSource    `json:"tree,omitempty"`
	Keys      *DocToolKeyMapSource  `json:"keys,omitempty"`
	Orders    *DocToolOrdersSource  `json:"orders,omitempty"`
	RootPlan  ContentHash           `json:"root_plan,omitempty"`
	RootCount int                   `json:"root_count"`
}

func (source docToolPolicySource) validate() error {
	if source.Version != 1 || source.Event.Validate() != nil || source.Event.Kind != EventToolCall && source.Event.Kind != EventToolResult || ValidateContentHash(source.Fields) != nil || source.RootCount < 0 || source.RootCount > source.Event.EventBytes {
		return ErrIntegrity
	}
	if source.Tree == nil {
		if source.Keys != nil || source.Orders != nil || source.RootPlan != "" || source.RootCount != 0 {
			return ErrIntegrity
		}
		return nil
	}
	if source.Tree.Validate() != nil || source.Tree.EventHash != source.Event.EventHash || source.Tree.EventBytes != source.Event.EventBytes || source.Tree.Evidence != source.Fields || source.Keys == nil || source.Orders == nil || source.Keys.Validate(*source.Tree) != nil || source.Orders.Validate(*source.Tree, *source.Keys) != nil {
		return ErrIntegrity
	}
	if source.Event.Kind == EventToolCall && source.Tree.Type == DocToolValueObject {
		return ValidateContentHash(source.RootPlan)
	}
	if source.RootPlan != "" || source.RootCount != 0 || source.Event.Kind == EventToolResult && source.Tree.Type != DocToolValueString && source.Tree.Type != DocToolValueNull {
		return ErrIntegrity
	}
	return nil
}

func (source docToolPolicySource) hash() (ContentHash, error) {
	if source.validate() != nil {
		return "", ErrIntegrity
	}
	raw, err := canonicalJSON(source)
	return HashContent(raw), err
}

type docToolEventPart struct {
	Kind      string            `json:"kind"`
	Token     DocFindSpan       `json:"token"`
	Text      string            `json:"text,omitempty"`
	Plan      ContentHash       `json:"plan,omitempty"`
	Skip      int               `json:"skip,omitempty"`
	Mode      DocToolStringMode `json:"mode,omitempty"`
	TextBytes int               `json:"text_bytes"`
}

func (part docToolEventPart) validate(source docToolPolicySource, complete bool) error {
	if part.Token.From < 0 || part.Token.To <= part.Token.From || part.Token.To > source.Event.EventBytes || part.TextBytes < 0 || part.TextBytes > MaxConversationDocumentBytes || part.Skip < 0 || part.Skip > MaxConversationDocumentBytes || part.Plan != "" && ValidateContentHash(part.Plan) != nil {
		return ErrIntegrity
	}
	if part.Kind == "literal" {
		if part.Plan != "" || part.Skip != 0 || part.Mode != "" || part.TextBytes != len(part.Text) {
			return ErrIntegrity
		}
		switch part.Text {
		case "tool", "(", ")", "…/", "\n", "\n$ ", "+ ", "− ":
			return nil
		default:
			return ErrIntegrity
		}
	}
	if part.Text != "" || complete && part.Plan == "" || part.Plan == "" && (part.TextBytes != 0 || part.Skip != 0) {
		return ErrIntegrity
	}
	switch part.Kind {
	case "decoded":
		if part.Token.To-part.Token.From < 2 || part.Mode != "" {
			return ErrIntegrity
		}
	case "diff":
		if part.Token.To-part.Token.From < 2 || part.Skip != 0 || part.Mode != DocToolStringAdd && part.Mode != DocToolStringRemove {
			return ErrIntegrity
		}
	case "value":
		if source.Tree == nil || part.Token != source.Tree.Root || part.Skip != 0 || part.Mode != "" {
			return ErrIntegrity
		}
	default:
		return ErrIntegrity
	}
	return nil
}

type docToolEventPlan struct {
	Version   int                 `json:"version"`
	Profile   string              `json:"profile"`
	Source    docToolPolicySource `json:"source"`
	Evidence  ContentHash         `json:"evidence"`
	Parts     []docToolEventPart  `json:"parts"`
	TextBytes int                 `json:"text_bytes"`
}

func (plan docToolEventPlan) encode() ([]byte, error) {
	if plan.Version != 1 || plan.Profile != docToolEventProfile || plan.Source.validate() != nil || ValidateContentHash(plan.Evidence) != nil || len(plan.Parts) > docToolEventParts || plan.TextBytes < 0 || plan.TextBytes > MaxConversationDocumentBytes {
		return nil, ErrIntegrity
	}
	total := 0
	for _, part := range plan.Parts {
		if part.validate(plan.Source, true) != nil || part.TextBytes > MaxConversationDocumentBytes-total {
			return nil, ErrIntegrity
		}
		total += part.TextBytes
	}
	if total != plan.TextBytes {
		return nil, ErrIntegrity
	}
	raw, err := canonicalJSON(plan)
	if err != nil || len(raw) > MaxDocDisplayStateBytes {
		return nil, ErrIntegrity
	}
	return raw, nil
}

func decodeDocToolEventPlan(expected ContentHash, raw []byte) (docToolEventPlan, error) {
	var plan docToolEventPlan
	if len(raw) == 0 || len(raw) > MaxDocDisplayStateBytes || HashContent(raw) != expected || json.Unmarshal(raw, &plan) != nil {
		return docToolEventPlan{}, ErrIntegrity
	}
	canonical, err := plan.encode()
	if err != nil || !bytes.Equal(canonical, raw) {
		return docToolEventPlan{}, ErrIntegrity
	}
	return plan, nil
}

func (plan docToolEventPlan) assembly(ctx context.Context, load func(context.Context, ContentHash) ([]byte, error)) (DocDisplayAssemblyInput, error) {
	encoded, err := plan.encode()
	if err != nil || load == nil {
		return DocDisplayAssemblyInput{}, ErrIntegrity
	}
	if err := plan.verifyEvidence(ctx, load); err != nil {
		return DocDisplayAssemblyInput{}, err
	}
	inputs := make([]DocDisplayAssemblyInput, 0, len(plan.Parts))
	for _, part := range plan.Parts {
		input, err := part.assembly(ctx, plan.Source, load)
		if err != nil || input.Source().TextBytes != part.TextBytes {
			if err != nil {
				return DocDisplayAssemblyInput{}, err
			}
			return DocDisplayAssemblyInput{}, ErrIntegrity
		}
		inputs = append(inputs, input)
	}
	source := DocDisplayProjectionSource{Version: 1, Profile: DocDisplayProjectionProfile, EventHash: plan.Source.Event.EventHash, EventBytes: plan.Source.Event.EventBytes, Plan: HashContent(encoded), TextBytes: plan.TextBytes}
	return newDocCompositeAssemblyInput(source, inputs)
}

func (plan docToolEventPlan) verifyEvidence(ctx context.Context, load func(context.Context, ContentHash) ([]byte, error)) error {
	reader := docEventDisplayBuilder{ctx: ctx, load: load}
	raw, err := reader.read(plan.Evidence, MaxDocDisplayStateBytes)
	if err != nil {
		return err
	}
	state, err := decodeDocToolPolicyState(plan.Evidence, raw)
	if err != nil || state.Phase != "complete" || state.Receipt == "" {
		return ErrIntegrity
	}
	source, sourceError := state.Source.hash()
	expectedSource, expectedError := plan.Source.hash()
	parts, partsError := canonicalJSON(state.Parts)
	expectedParts, expectedPartsError := canonicalJSON(plan.Parts)
	if sourceError != nil || expectedError != nil || partsError != nil || expectedPartsError != nil || source != expectedSource || !bytes.Equal(parts, expectedParts) {
		return ErrIntegrity
	}
	raw, err = reader.read(state.Receipt, MaxDocDisplaySourceBytes)
	if err != nil {
		return err
	}
	if err := validateDocToolPolicyReceipt(state, raw); err != nil {
		return err
	}
	if plan.Source.RootPlan != "" {
		_, err = reader.toolPolicyRoot(plan.Source)
	}
	return err
}

func (part docToolEventPart) assembly(ctx context.Context, source docToolPolicySource, load func(context.Context, ContentHash) ([]byte, error)) (DocDisplayAssemblyInput, error) {
	if err := ctx.Err(); err != nil {
		return DocDisplayAssemblyInput{}, err
	}
	if part.Kind == "literal" {
		return newDocToolLiteralAssemblyInput(source.Event.EventHash, source.Event.EventBytes, part.Text, part.Token)
	}
	raw, err := load(ctx, part.Plan)
	if err != nil {
		return DocDisplayAssemblyInput{}, err
	}
	if len(raw) > MaxDocDisplaySourceBytes || HashContent(raw) != part.Plan {
		return DocDisplayAssemblyInput{}, ErrIntegrity
	}
	switch part.Kind {
	case "decoded":
		generation, err := DecodeDocDecodedTextGeneration(part.Plan, raw)
		if err != nil || generation.Source.EventHash != source.Event.EventHash || generation.Source.EventBytes != source.Event.EventBytes || generation.Source.Token != part.Token {
			return DocDisplayAssemblyInput{}, ErrIntegrity
		}
		return newDocToolDecodedAssemblyInput(generation, part.Skip)
	case "diff":
		plan, err := DecodeDocToolStringPlan(part.Plan, raw)
		if err != nil || plan.Source.EventHash != source.Event.EventHash || plan.Source.EventBytes != source.Event.EventBytes || plan.Source.Token != part.Token || plan.Source.Mode != part.Mode {
			return DocDisplayAssemblyInput{}, ErrIntegrity
		}
		return NewDocToolStringAssemblyInput(plan)
	case "value":
		plan, err := DecodeDocToolValuePlan(part.Plan, raw)
		if err != nil || source.Tree == nil || source.Keys == nil || source.Orders == nil || plan.Source.Tree != *source.Tree || plan.Source.Keys != *source.Keys || plan.Source.Orders != *source.Orders {
			return DocDisplayAssemblyInput{}, ErrIntegrity
		}
		return NewDocToolValueAssemblyInput(plan)
	default:
		return DocDisplayAssemblyInput{}, ErrIntegrity
	}
}
