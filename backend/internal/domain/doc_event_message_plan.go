package domain

import "context"

func (builder *docEventDisplayBuilder) messagePlan(load func(context.Context, int, int) ([]byte, error), inputReader *docMessageInputReader) error {
	if inputReader == nil {
		return ErrIntegrity
	}
	inputs, err := builder.messageInputs()
	if err != nil {
		return err
	}
	source := DocMessageDisplaySource{Version: 1, Profile: DocMessageDisplayProfile, Input: inputs}
	raw, err := builder.read(builder.state.Active, MaxDocDisplayStateBytes)
	if err != nil {
		return err
	}
	state, err := DecodeDocMessageDisplayState(source, builder.state.Active, raw)
	if err != nil {
		return err
	}
	if err := ValidateDocMessageDisplayCheckpoint(builder.ctx, source, state, builder.loadObject); err != nil {
		return err
	}
	if state.Done(source) {
		if builder.state.Child != "" {
			return ErrIntegrity
		}
		return builder.finishMessagePlan(source, state)
	}
	inputReader.generation = inputs
	page, err := inputReader.read(builder.ctx, state.Next/DocMessageInputPageEntries, builder.loadObject)
	if err != nil {
		return err
	}
	input := page[state.Next%DocMessageInputPageEntries]
	if builder.state.Child == "" {
		if input.IsText() {
			selected, err := input.TextSource()
			if err != nil {
				return err
			}
			if selected == nil {
				children := make([]DocMessageDisplayBlockPlan, 0, DocMessageDisplayStepBlocks)
				for _, candidate := range page[state.Next%DocMessageInputPageEntries:] {
					if !candidate.IsText() {
						break
					}
					selected, err := candidate.TextSource()
					if err != nil {
						return err
					}
					if selected != nil {
						break
					}
					children = append(children, DocMessageDisplayBlockPlan{})
				}
				return builder.appendMessagePlans(source, state, children)
			}
		}
		builder.state.Child, err = builder.startMessageChild(builder.state.Inputs, input)
		return err
	}
	child, plan, done, err := builder.advanceMessageChild(builder.state.Inputs, input, builder.state.Child, load)
	if err != nil {
		return err
	}
	builder.state.Child = child
	if !done {
		return nil
	}
	return builder.appendMessagePlans(source, state, []DocMessageDisplayBlockPlan{plan})
}

func (builder *docEventDisplayBuilder) appendMessagePlans(source DocMessageDisplaySource, prior DocMessageDisplayState, children []DocMessageDisplayBlockPlan) error {
	step, err := StepDocMessageDisplayPlan(builder.ctx, source, prior, children, builder.loadObject)
	if err != nil {
		return err
	}
	if step.Processed <= 0 || step.Processed > len(children) {
		return ErrIntegrity
	}
	if err := builder.append(step.Objects); err != nil {
		return err
	}
	if step.State.Done(source) {
		return builder.finishMessagePlan(source, step.State)
	}
	builder.state.Active, err = builder.put(step.State.Encode(source))
	return err
}

func (builder *docEventDisplayBuilder) finishMessagePlan(source DocMessageDisplaySource, state DocMessageDisplayState) error {
	plan, objects, err := FinishDocMessageDisplayPlan(builder.ctx, source, state, builder.loadObject)
	if err != nil {
		return err
	}
	if err := builder.append(objects); err != nil {
		return err
	}
	builder.state.Plan, err = builder.put(plan.Encode())
	if err != nil {
		return err
	}
	return builder.beginAssembly()
}
