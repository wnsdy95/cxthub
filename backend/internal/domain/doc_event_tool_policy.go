package domain

import (
	"bytes"
	"context"
	"encoding/json"
)

func (builder *docEventDisplayBuilder) toolPolicyFields(source docToolPolicySource) (DocToolFields, error) {
	discovery, err := NewDocToolFieldDiscoverySource(source.Event.EventHash, source.Event.EventBytes)
	if err != nil {
		return DocToolFields{}, err
	}
	raw, err := builder.read(source.Fields, MaxDocFindFieldsBytes)
	if err != nil {
		return DocToolFields{}, err
	}
	fields, err := DecodeDocToolFields(discovery, source.Fields, raw)
	if err != nil || fields.Kind != source.Event.Kind {
		return DocToolFields{}, ErrIntegrity
	}
	tree, err := docToolTreeSourceFromFields(discovery, fields)
	if source.Tree == nil {
		if err != ErrDocFindDisplayUnavailable {
			return DocToolFields{}, ErrIntegrity
		}
	} else if err != nil || tree != *source.Tree {
		return DocToolFields{}, ErrIntegrity
	}
	return fields, nil
}

func (source docToolPolicySource) matches(state DocEventDisplayPreparation) bool {
	if source.Event != state.Source || source.Fields != state.ToolFields || (source.Tree == nil) != (state.ToolTree == nil) || (source.Keys == nil) != (state.ToolKeys == nil) || (source.Orders == nil) != (state.ToolOrders == nil) {
		return false
	}
	return source.Tree == nil || *source.Tree == *state.ToolTree && *source.Keys == *state.ToolKeys && *source.Orders == *state.ToolOrders
}

func (builder *docEventDisplayBuilder) toolPolicyInitialize(tools DocToolPreparationReader) error {
	source := docToolPolicySource{Version: 1, Event: builder.state.Source, Fields: builder.state.ToolFields, Tree: builder.state.ToolTree, Keys: builder.state.ToolKeys, Orders: builder.state.ToolOrders}
	if source.Tree != nil {
		raw, err := builder.read(builder.state.Active, MaxDocDisplayStateBytes)
		if err != nil {
			return err
		}
		orders, err := DecodeDocToolOrdersState(*source.Tree, *source.Keys, *source.Orders, builder.state.Active, raw)
		if err != nil || !orders.Done(*source.Orders) {
			return ErrIntegrity
		}
	}
	if source.Event.Kind == EventToolCall && source.Tree != nil && source.Tree.Type == DocToolValueObject {
		if tools.OrderPlan == nil {
			return ErrDocFindDisplayUnavailable
		}
		plan, err := tools.OrderPlan(builder.ctx, source.Tree.Root.From)
		if err != nil {
			return err
		}
		if plan.Source.Parent.Depth != 0 || plan.Source.Parent.Value != source.Tree.Root {
			return ErrIntegrity
		}
		source.RootPlan, err = builder.put(plan.Encode(*source.Tree, *source.Keys))
		if err != nil {
			return err
		}
		source.RootCount = plan.Source.Count
	}
	if _, err := builder.toolPolicyFields(source); err != nil {
		return err
	}
	state := docToolPolicyState{Version: 1, Source: source, Phase: "select"}
	var err error
	builder.state.Active, err = builder.put(state.encode())
	builder.state.Phase = "tool_policy"
	return err
}

func (builder *docEventDisplayBuilder) toolPolicyRoot(source docToolPolicySource) (DocToolOrderPlan, error) {
	if source.Tree == nil || source.Keys == nil {
		return DocToolOrderPlan{}, ErrIntegrity
	}
	raw, err := builder.read(source.RootPlan, MaxDocDisplaySourceBytes)
	if err != nil {
		return DocToolOrderPlan{}, err
	}
	plan, err := DecodeDocToolOrderPlan(*source.Tree, *source.Keys, source.RootPlan, raw)
	if err != nil || plan.Source.Parent.Depth != 0 || plan.Source.Parent.Value != source.Tree.Root || plan.Source.Count != source.RootCount {
		return DocToolOrderPlan{}, ErrIntegrity
	}
	return plan, nil
}

func (builder *docEventDisplayBuilder) toolPolicyCheckpoint(state docToolPolicyState) error {
	if !state.Source.matches(builder.state) {
		return ErrIntegrity
	}
	if state.Receipt == "" {
		if state.Source.RootPlan != "" {
			if _, err := builder.toolPolicyRoot(state.Source); err != nil {
				return err
			}
		}
		initial := docToolPolicyState{Version: 1, Source: state.Source, Phase: "select"}
		raw, err := state.encode()
		expected, other := initial.encode()
		if err != nil || other != nil || !bytes.Equal(raw, expected) {
			return ErrIntegrity
		}
		return nil
	}
	raw, err := builder.read(state.Receipt, MaxDocDisplaySourceBytes)
	if err != nil {
		return err
	}
	return validateDocToolPolicyReceipt(state, raw)
}

func validateDocToolPolicyReceipt(state docToolPolicyState, raw []byte) error {
	var receipt docToolPolicyReceipt
	if json.Unmarshal(raw, &receipt) != nil {
		return ErrIntegrity
	}
	canonical, err := canonicalJSON(receipt)
	digest, digestError := state.digest()
	source, sourceError := state.Source.hash()
	if err != nil || digestError != nil || sourceError != nil || !bytes.Equal(raw, canonical) || receipt != (docToolPolicyReceipt{Version: 1, Profile: docToolEventProfile, Source: source, Digest: digest}) {
		return ErrIntegrity
	}
	return nil
}

func (builder *docEventDisplayBuilder) toolPolicyStep(loadRaw func(context.Context, int, int) ([]byte, error), tools DocToolPreparationReader) error {
	raw, err := builder.read(builder.state.Active, MaxDocDisplayStateBytes)
	if err != nil {
		return err
	}
	state, err := decodeDocToolPolicyState(builder.state.Active, raw)
	if err != nil {
		return err
	}
	if err := builder.toolPolicyCheckpoint(state); err != nil {
		return err
	}
	fields, err := builder.toolPolicyFields(state.Source)
	if err != nil {
		return err
	}
	if state.Phase == "complete" {
		plan := docToolEventPlan{Version: 1, Profile: docToolEventProfile, Source: state.Source, Evidence: builder.state.Active, Parts: state.Parts}
		for _, part := range plan.Parts {
			if part.TextBytes > MaxConversationDocumentBytes-plan.TextBytes {
				return ErrDocFindDisplayUnavailable
			}
			plan.TextBytes += part.TextBytes
		}
		builder.state.ToolEvent, err = builder.put(plan.encode())
		if err != nil {
			return err
		}
		builder.state.Plan = builder.state.ToolEvent
		input, err := plan.assembly(builder.ctx, builder.loadObject)
		if err != nil {
			return err
		}
		assembly, err := NewDocDisplayAssemblyState(input)
		if err != nil {
			return err
		}
		builder.state.Active, err = builder.put(assembly.Encode(input))
		builder.state.Phase = "tool_event_assembly"
		return err
	}
	switch state.Phase {
	case "select":
		err = builder.toolPolicySelect(&state)
	case "name", "head":
		err = builder.toolPolicyHeader(&state, fields, loadRaw)
	case "truth":
		err = builder.toolPolicyTruth(&state, loadRaw)
	case "layout":
		err = state.layout(fields)
	case "parts":
		err = builder.toolPolicyPart(&state, loadRaw, tools)
	}
	if err != nil {
		return err
	}
	digest, err := state.digest()
	if err != nil {
		return err
	}
	source, err := state.Source.hash()
	if err != nil {
		return err
	}
	state.Receipt, err = builder.put(canonicalJSON(docToolPolicyReceipt{Version: 1, Profile: docToolEventProfile, Source: source, Digest: digest}))
	if err != nil {
		return err
	}
	builder.state.Active, err = builder.put(state.encode())
	return err
}

func (builder *docEventDisplayBuilder) toolPolicySelect(state *docToolPolicyState) error {
	if state.Page == (state.Source.RootCount+DocToolOrderPageEntries-1)/DocToolOrderPageEntries {
		state.Phase = "name"
		return nil
	}
	plan, err := builder.toolPolicyRoot(state.Source)
	if err != nil {
		return err
	}
	page, err := ReadDocToolOrderPage(builder.ctx, *state.Source.Tree, *state.Source.Keys, plan, state.Page, builder.loadObject)
	if err != nil {
		return err
	}
	for _, entry := range page.Entries {
		for index, name := range docToolPolicyKeys {
			if entry.Digest == HashContent([]byte(`"`+name+`"`)) {
				if state.Members[index] != nil {
					return ErrIntegrity
				}
				node := cloneDocToolTreeNode(entry.Node)
				state.Members[index] = &node
			}
		}
	}
	state.Page++
	return nil
}
