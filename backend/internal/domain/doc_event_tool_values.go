package domain

import "context"

func (builder *docEventDisplayBuilder) toolValuesInitialize(tools DocToolPreparationReader) error {
	tree, keys, orders := *builder.state.ToolTree, *builder.state.ToolKeys, *builder.state.ToolOrders
	raw, err := builder.read(builder.state.Active, MaxDocDisplayStateBytes)
	if err != nil {
		return err
	}
	complete, err := DecodeDocToolOrdersState(tree, keys, orders, builder.state.Active, raw)
	if err != nil || !complete.Done(orders) {
		return ErrIntegrity
	}
	nodes, err := tools.Nodes(builder.ctx, tree.Root.From-1, 1)
	if err != nil {
		return err
	}
	if len(nodes) != 1 || nodes[0].Depth != 0 {
		return ErrIntegrity
	}
	source, err := NewDocToolValueSource(tree, keys, orders, nodes[0])
	if err != nil {
		return err
	}
	state, err := NewDocToolValueState(source)
	if err != nil {
		return err
	}
	builder.state.ToolValue, err = builder.put(source.Encode())
	if err != nil {
		return err
	}
	builder.state.Active, err = builder.put(state.Encode(source))
	builder.state.Phase = "tool_values"
	return err
}

func (builder *docEventDisplayBuilder) toolValuesStep(loadRaw func(context.Context, int, int) ([]byte, error), loadOrder func(context.Context, int) (DocToolOrderPlan, error)) error {
	raw, err := builder.read(builder.state.ToolValue, MaxDocDisplaySourceBytes)
	if err != nil {
		return err
	}
	source, err := DecodeDocToolValueSource(builder.state.ToolValue, raw)
	if err != nil || source.Tree != *builder.state.ToolTree || source.Keys != *builder.state.ToolKeys || source.Orders != *builder.state.ToolOrders {
		return ErrIntegrity
	}
	raw, err = builder.read(builder.state.Active, MaxDocDisplayStateBytes)
	if err != nil {
		return err
	}
	state, err := DecodeDocToolValueState(source, builder.state.Active, raw)
	if err != nil {
		return err
	}
	if !state.Done() {
		step, err := StepDocToolValue(builder.ctx, source, state, loadRaw, builder.loadObject, loadOrder)
		if err != nil {
			return err
		}
		if err := builder.append(step.Objects()); err != nil {
			return err
		}
		builder.state.Active, err = builder.put(step.State().Encode(source))
		return err
	}
	plan, objects, err := FinishDocToolValue(builder.ctx, source, state, builder.loadObject)
	if err != nil {
		return err
	}
	if err := builder.append(objects); err != nil {
		return err
	}
	builder.state.ToolPlan, err = builder.put(plan.Encode())
	builder.state.Phase = "tool_values_done"
	return err
}
