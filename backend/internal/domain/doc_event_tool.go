package domain

import (
	"context"
	"errors"
)

func (state DocEventDisplayPreparation) validateTool() error {
	if state.Phase == "tool_policy" || state.Phase == "tool_event_assembly" || state.Phase == "completed" && state.ToolEvent != "" {
		return state.validateToolPolicy()
	}
	if state.ToolEvent != "" {
		return ErrIntegrity
	}
	publication := state.Phase == "tool_assembly" || state.Phase == "completed"
	if state.Input != nil || state.Message != nil || state.Decoded != "" || state.Inputs != "" || state.Child != "" {
		return ErrIntegrity
	}
	if publication {
		if state.Source.Kind != EventToolResult || state.ToolTree == nil || state.ToolTree.Type == DocToolValueString || state.ToolTree.Type == DocToolValueNull || state.Plan == "" || state.Plan != state.ToolPlan || (state.Phase == "completed") != (state.Generation != "") || (state.Phase == "completed") != (state.Active == "") {
			return ErrIntegrity
		}
	} else if state.Plan != "" || state.Generation != "" {
		return ErrIntegrity
	}
	values := publication || state.Phase == "tool_values" || state.Phase == "tool_values_done"
	if values != (state.ToolValue != "") || (publication || state.Phase == "tool_values_done") != (state.ToolPlan != "") {
		return ErrIntegrity
	}
	switch state.Phase {
	case "tool_discovery":
		if state.ToolFields != "" || state.ToolTree != nil || state.ToolKeys != nil || state.ToolOrders != nil {
			return ErrIntegrity
		}
	case "tool_tree", "tool_tree_done", "tool_keys", "tool_keys_done", "tool_orders", "tool_orders_done", "tool_values", "tool_values_done", "tool_assembly", "completed":
		if state.Phase != "completed" && state.Active == "" || state.ToolFields == "" || state.ToolTree == nil || state.ToolTree.Validate() != nil || state.ToolTree.EventHash != state.Source.EventHash || state.ToolTree.EventBytes != state.Source.EventBytes || state.ToolTree.Evidence != state.ToolFields {
			return ErrIntegrity
		}
		orders := values || state.Phase == "tool_orders" || state.Phase == "tool_orders_done"
		keys := orders || state.Phase == "tool_keys" || state.Phase == "tool_keys_done"
		if keys != (state.ToolKeys != nil) || keys && state.ToolKeys.Validate(*state.ToolTree) != nil {
			return ErrIntegrity
		}
		if orders != (state.ToolOrders != nil) || orders && state.ToolOrders.Validate(*state.ToolTree, *state.ToolKeys) != nil {
			return ErrIntegrity
		}
	case "tool_empty":
		if state.Active != "" || state.ToolFields == "" || state.ToolTree != nil || state.ToolKeys != nil || state.ToolOrders != nil {
			return ErrIntegrity
		}
	default:
		return ErrIntegrity
	}
	return nil
}

func (state DocEventDisplayPreparation) validateToolPolicy() error {
	if state.Input != nil || state.Message != nil || state.Decoded != "" || state.Inputs != "" || state.Child != "" || state.ToolValue != "" || state.ToolPlan != "" || state.ToolFields == "" {
		return ErrIntegrity
	}
	if state.ToolTree == nil {
		if state.ToolKeys != nil || state.ToolOrders != nil {
			return ErrIntegrity
		}
	} else if state.ToolTree.Validate() != nil || state.ToolTree.Evidence != state.ToolFields || state.ToolTree.EventHash != state.Source.EventHash || state.ToolTree.EventBytes != state.Source.EventBytes || state.ToolKeys == nil || state.ToolOrders == nil || state.ToolKeys.Validate(*state.ToolTree) != nil || state.ToolOrders.Validate(*state.ToolTree, *state.ToolKeys) != nil {
		return ErrIntegrity
	}
	if state.Phase == "tool_policy" {
		if state.Active == "" || state.ToolEvent != "" || state.Plan != "" || state.Generation != "" {
			return ErrIntegrity
		}
		return nil
	}
	if state.ToolEvent == "" || state.Plan != state.ToolEvent || (state.Phase == "completed") != (state.Generation != "") || (state.Phase == "completed") != (state.Active == "") {
		return ErrIntegrity
	}
	return nil
}

func (builder *docEventDisplayBuilder) toolKeysInitialize() error {
	tree := *builder.state.ToolTree
	raw, err := builder.read(builder.state.Active, MaxDocToolTreeScanStateBytes)
	if err != nil {
		return err
	}
	state, err := DecodeDocToolTreeScanState(tree, builder.state.Active, raw)
	if err != nil {
		return err
	}
	complete, err := FinishDocToolTreeScan(builder.ctx, tree, state, builder.loadObject)
	if err != nil {
		return err
	}
	source, err := NewDocToolKeyMapSource(tree, complete)
	if err != nil {
		return err
	}
	keys, err := NewDocToolKeyMapState(tree, source)
	if err != nil {
		return err
	}
	builder.state.ToolKeys, builder.state.Phase = &source, "tool_keys"
	builder.state.Active, err = builder.put(keys.Encode(tree, source))
	return err
}

func (builder *docEventDisplayBuilder) toolKeysStep(load func(context.Context, int, int) ([]byte, error), nodes DocToolTreeNodeLoader) error {
	tree, source := *builder.state.ToolTree, *builder.state.ToolKeys
	raw, err := builder.read(builder.state.Active, MaxDocDisplayStateBytes)
	if err != nil {
		return err
	}
	state, err := DecodeDocToolKeyMapState(tree, source, builder.state.Active, raw)
	if err != nil {
		return err
	}
	step, err := StepDocToolKeyMap(builder.ctx, tree, source, state, load, builder.loadObject, nodes)
	if err != nil {
		return err
	}
	if err := builder.append(step.Objects()); err != nil {
		return err
	}
	next := step.State()
	builder.state.Active, err = builder.put(next.Encode(tree, source))
	if err != nil {
		return err
	}
	if next.Done() {
		builder.state.Phase = "tool_keys_done"
	}
	builder.keys = &step
	return nil
}

func (builder *docEventDisplayBuilder) toolDiscover(load func(context.Context, int, int) ([]byte, error)) error {
	source, err := NewDocToolFieldDiscoverySource(builder.state.Source.EventHash, builder.state.Source.EventBytes)
	if err != nil {
		return err
	}
	var discovery DocToolFieldDiscoveryState
	if builder.state.Active == "" {
		discovery, err = NewDocToolFieldDiscoveryState(source)
	} else {
		raw, failure := builder.read(builder.state.Active, MaxDocFieldDiscoveryStateBytes)
		if failure != nil {
			return failure
		}
		discovery, err = DecodeDocToolFieldDiscoveryState(source, builder.state.Active, raw)
	}
	if err != nil {
		return err
	}
	length := min(DocFieldDiscoveryStepBytes, source.EventBytes-discovery.Syntax.Next)
	raw, err := load(builder.ctx, discovery.Syntax.Next, length)
	if err != nil {
		return err
	}
	if len(raw) != length {
		return ErrIntegrity
	}
	step, err := AdvanceDocToolFieldDiscovery(builder.ctx, source, discovery, raw)
	if err != nil {
		return err
	}
	discovery, err = step.State(source)
	if err != nil {
		return err
	}
	if !discovery.Syntax.Done {
		builder.state.Active, err = builder.put(discovery.Encode(source))
		return err
	}
	fields, err := discovery.Fields(source)
	if err != nil || fields.Kind != builder.state.Source.Kind {
		return ErrIntegrity
	}
	builder.state.ToolFields, err = builder.put(fields.Encode(source))
	if err != nil {
		return err
	}
	tree, err := NewDocToolTreeSource(source, discovery, DocRangeEntry{DocEventIndex: DocEventIndex{Hash: builder.state.Source.EventHash, Length: builder.state.Source.EventBytes}, Kind: builder.state.Source.Kind})
	if errors.Is(err, ErrDocFindDisplayUnavailable) {
		builder.state.Active, builder.state.Phase = "", "tool_empty"
		return nil
	}
	if err != nil {
		return err
	}
	state, err := NewDocToolTreeScanState(tree)
	if err != nil {
		return err
	}
	builder.state.ToolTree, builder.state.Phase = &tree, "tool_tree"
	builder.state.Active, err = builder.put(state.Encode(tree))
	return err
}

func (builder *docEventDisplayBuilder) toolTreeScan(load func(context.Context, int, int) ([]byte, error)) error {
	source := *builder.state.ToolTree
	raw, err := builder.read(builder.state.Active, MaxDocDisplayStateBytes)
	if err != nil {
		return err
	}
	state, err := DecodeDocToolTreeScanState(source, builder.state.Active, raw)
	if err != nil {
		return err
	}
	step, err := StepDocToolTreeScan(builder.ctx, source, state, load, builder.loadObject)
	if err != nil {
		return err
	}
	if err := builder.append(step.Objects()); err != nil {
		return err
	}
	next := step.State()
	builder.state.Active, err = builder.put(next.Encode(source))
	if err != nil {
		return err
	}
	if next.Done() {
		builder.state.Phase = "tool_tree_done"
	}
	builder.tool = &step
	return nil
}
