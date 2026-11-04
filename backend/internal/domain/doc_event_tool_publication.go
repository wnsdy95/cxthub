package domain

func (builder *docEventDisplayBuilder) toolResultAssemblyInput() (DocDisplayAssemblyInput, error) {
	state := builder.state
	if state.Source.Kind != EventToolResult || state.ToolTree == nil || state.ToolKeys == nil || state.ToolOrders == nil || state.Plan != state.ToolPlan || state.ToolPlan == "" {
		return DocDisplayAssemblyInput{}, ErrIntegrity
	}
	discovery, err := NewDocToolFieldDiscoverySource(state.Source.EventHash, state.Source.EventBytes)
	if err != nil {
		return DocDisplayAssemblyInput{}, err
	}
	raw, err := builder.read(state.ToolFields, MaxDocFindFieldsBytes)
	if err != nil {
		return DocDisplayAssemblyInput{}, err
	}
	fields, err := DecodeDocToolFields(discovery, state.ToolFields, raw)
	if err != nil || fields.Kind != EventToolResult || fields.Output == nil || fields.OutputType == DocToolValueString || fields.OutputType == DocToolValueNull {
		return DocDisplayAssemblyInput{}, ErrIntegrity
	}
	tree, err := docToolTreeSourceFromFields(discovery, fields)
	if err != nil || tree != *state.ToolTree {
		return DocDisplayAssemblyInput{}, ErrIntegrity
	}
	raw, err = builder.read(state.ToolValue, MaxDocDisplaySourceBytes)
	if err != nil {
		return DocDisplayAssemblyInput{}, err
	}
	source, err := DecodeDocToolValueSource(state.ToolValue, raw)
	if err != nil || source.Tree != tree || source.Keys != *state.ToolKeys || source.Orders != *state.ToolOrders {
		return DocDisplayAssemblyInput{}, ErrIntegrity
	}
	raw, err = builder.read(state.ToolPlan, MaxDocDisplaySourceBytes)
	if err != nil {
		return DocDisplayAssemblyInput{}, err
	}
	plan, err := DecodeDocToolValuePlan(state.ToolPlan, raw)
	if err != nil || !sameDocToolValueSource(plan.Source, source) {
		return DocDisplayAssemblyInput{}, ErrIntegrity
	}
	return NewDocToolValueAssemblyInput(plan)
}

func sameDocToolValueSource(left, right DocToolValueSource) bool {
	leftHash, leftError := left.Hash()
	rightHash, rightError := right.Hash()
	return leftError == nil && rightError == nil && leftHash == rightHash
}

func (builder *docEventDisplayBuilder) toolResultAssembly() error {
	if builder.state.Source.Kind != EventToolResult || builder.state.ToolTree == nil || builder.state.ToolTree.Type == DocToolValueString || builder.state.ToolTree.Type == DocToolValueNull {
		return ErrDocFindDisplayUnavailable
	}
	builder.state.Plan = builder.state.ToolPlan
	input, err := builder.toolResultAssemblyInput()
	if err != nil {
		return err
	}
	state, err := NewDocDisplayAssemblyState(input)
	if err != nil {
		return err
	}
	builder.state.Active, err = builder.put(state.Encode(input))
	builder.state.Phase = "tool_assembly"
	return err
}
