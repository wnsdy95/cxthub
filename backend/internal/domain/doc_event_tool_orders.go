package domain

func (builder *docEventDisplayBuilder) toolOrdersInitialize() error {
	tree, keys := *builder.state.ToolTree, *builder.state.ToolKeys
	raw, err := builder.read(builder.state.Active, MaxDocDisplayStateBytes)
	if err != nil {
		return err
	}
	complete, err := DecodeDocToolKeyMapState(tree, keys, builder.state.Active, raw)
	if err != nil || !complete.Done() {
		return ErrIntegrity
	}
	source, err := NewDocToolOrdersSource(tree, keys)
	if err != nil {
		return err
	}
	state, err := NewDocToolOrdersState(tree, keys, source)
	if err != nil {
		return err
	}
	builder.state.ToolOrders, builder.state.Phase = &source, "tool_orders"
	builder.state.Active, err = builder.put(state.Encode(tree, keys, source))
	return err
}

func (builder *docEventDisplayBuilder) toolOrdersStep(tools DocToolPreparationReader) error {
	tree, keys, source := *builder.state.ToolTree, *builder.state.ToolKeys, *builder.state.ToolOrders
	raw, err := builder.read(builder.state.Active, MaxDocDisplayStateBytes)
	if err != nil {
		return err
	}
	state, err := DecodeDocToolOrdersState(tree, keys, source, builder.state.Active, raw)
	if err != nil {
		return err
	}
	step, err := StepDocToolOrders(builder.ctx, tree, keys, source, state, tools, builder.loadObject)
	if err != nil {
		return err
	}
	if err := builder.append(step.Objects()); err != nil {
		return err
	}
	next := step.State()
	builder.state.Active, err = builder.put(next.Encode(tree, keys, source))
	if err != nil {
		return err
	}
	if next.Done(source) {
		builder.state.Phase = "tool_orders_done"
	}
	builder.orders = &step
	return nil
}
