package domain

import "context"

func (builder *docEventDisplayBuilder) toolPolicyPart(state *docToolPolicyState, loadRaw func(context.Context, int, int) ([]byte, error), tools DocToolPreparationReader) error {
	if state.Part == len(state.Parts) {
		state.Phase = "complete"
		return nil
	}
	part := &state.Parts[state.Part]
	if part.Kind == "literal" || part.Plan != "" {
		state.Part++
		return nil
	}
	switch part.Kind {
	case "decoded":
		for _, text := range []*docToolPolicyText{state.Name, state.Head} {
			if text != nil && text.Token == part.Token {
				part.Plan, part.TextBytes = text.Generation, text.Bytes
				state.Part++
				return nil
			}
		}
		complete, err := builder.toolPolicyDecode(state, part.Token, loadRaw)
		if err != nil || !complete {
			return err
		}
		if state.Source.Event.Kind == EventToolResult && !state.Stats.NonSpace {
			state.Parts = nil
		} else {
			part.Plan, part.TextBytes = state.Stats.Generation, state.Stats.Bytes
			state.Part++
		}
		state.Stats = nil
		return nil
	case "diff":
		return builder.toolPolicyDiff(state, loadRaw)
	case "value":
		return builder.toolPolicyValue(state, loadRaw, tools)
	default:
		return ErrIntegrity
	}
}

func (builder *docEventDisplayBuilder) toolPolicyDiff(state *docToolPolicyState, loadRaw func(context.Context, int, int) ([]byte, error)) error {
	part := &state.Parts[state.Part]
	source := DocToolStringSource{Version: 1, Profile: DocToolStringProfile, EventHash: state.Source.Event.EventHash, EventBytes: state.Source.Event.EventBytes, Token: part.Token, Mode: part.Mode}
	if state.Active == "" {
		stringState, err := NewDocToolStringState(source)
		if err != nil {
			return err
		}
		state.Active, err = builder.put(stringState.Encode(source))
		return err
	}
	raw, err := builder.read(state.Active, MaxDocDisplayStateBytes)
	if err != nil {
		return err
	}
	stringState, err := DecodeDocToolStringState(source, state.Active, raw)
	if err != nil {
		return err
	}
	if !stringState.Done {
		step, err := StepDocToolString(builder.ctx, source, stringState, loadRaw, builder.loadObject)
		if err != nil {
			return err
		}
		if err := builder.append(step.Objects); err != nil {
			return err
		}
		state.Active, err = builder.put(step.State.Encode(source))
		return err
	}
	plan, objects, err := FinishDocToolString(builder.ctx, source, stringState, builder.loadObject)
	if err != nil {
		return err
	}
	if err := builder.append(objects); err != nil {
		return err
	}
	part.Plan, err = builder.put(plan.Encode())
	part.TextBytes = plan.TextBytes
	state.Active = ""
	state.Part++
	return err
}

func (builder *docEventDisplayBuilder) toolPolicyValue(state *docToolPolicyState, loadRaw func(context.Context, int, int) ([]byte, error), tools DocToolPreparationReader) error {
	if tools.OrderPlan == nil {
		return ErrDocFindDisplayUnavailable
	}
	root, err := builder.toolPolicyRoot(state.Source)
	if err != nil {
		return err
	}
	source, err := NewDocToolValueSource(*state.Source.Tree, *state.Source.Keys, *state.Source.Orders, root.Source.Parent)
	if err != nil {
		return err
	}
	if state.Active == "" {
		value, err := NewDocToolValueState(source)
		if err != nil {
			return err
		}
		state.Active, err = builder.put(value.Encode(source))
		return err
	}
	raw, err := builder.read(state.Active, MaxDocDisplayStateBytes)
	if err != nil {
		return err
	}
	value, err := DecodeDocToolValueState(source, state.Active, raw)
	if err != nil {
		return err
	}
	if !value.Done() {
		step, err := StepDocToolValue(builder.ctx, source, value, loadRaw, builder.loadObject, tools.OrderPlan)
		if err != nil {
			return err
		}
		if err := builder.append(step.Objects()); err != nil {
			return err
		}
		state.Active, err = builder.put(step.State().Encode(source))
		return err
	}
	plan, objects, err := FinishDocToolValue(builder.ctx, source, value, builder.loadObject)
	if err != nil {
		return err
	}
	if err := builder.append(objects); err != nil {
		return err
	}
	part := &state.Parts[state.Part]
	part.Plan, err = builder.put(plan.Encode())
	part.TextBytes = plan.TextBytes
	state.Active = ""
	state.Part++
	return err
}

func (builder *docEventDisplayBuilder) toolEventAssemblyInput() (DocDisplayAssemblyInput, error) {
	if builder.state.ToolEvent == "" || builder.state.Plan != builder.state.ToolEvent {
		return DocDisplayAssemblyInput{}, ErrIntegrity
	}
	raw, err := builder.read(builder.state.ToolEvent, MaxDocDisplayStateBytes)
	if err != nil {
		return DocDisplayAssemblyInput{}, err
	}
	plan, err := decodeDocToolEventPlan(builder.state.ToolEvent, raw)
	if err != nil || !plan.Source.matches(builder.state) {
		return DocDisplayAssemblyInput{}, ErrIntegrity
	}
	if _, err := builder.toolPolicyFields(plan.Source); err != nil {
		return DocDisplayAssemblyInput{}, err
	}
	return plan.assembly(builder.ctx, builder.loadObject)
}
