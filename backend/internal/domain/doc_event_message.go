package domain

import "context"

func (state DocEventDisplayPreparation) validateMessage() error {
	if state.Input != nil || state.Message != nil && (state.Message.Validate() != nil || state.Message.EventHash != state.Source.EventHash || state.Message.EventBytes != state.Source.EventBytes || state.Message.Summary != state.Source.Summary) {
		return ErrIntegrity
	}
	valid := false
	switch state.Phase {
	case "discovery":
		valid = state.Message == nil && state.Inputs == "" && state.Child == "" && state.Decoded == "" && state.Plan == "" && state.Generation == ""
	case "message_inputs":
		valid = state.Message != nil && state.Active != "" && state.Inputs == "" && state.Child == "" && state.Decoded == "" && state.Plan == "" && state.Generation == ""
	case "summary_text":
		valid = state.Source.Summary && state.Message == nil && state.Inputs != "" && state.Active != "" && state.Child == "" && state.Decoded == "" && state.Plan == "" && state.Generation == ""
	case "markdown":
		valid = state.Source.Summary && state.Message == nil && state.Inputs != "" && state.Active != "" && state.Child == "" && state.Decoded != "" && state.Plan == "" && state.Generation == ""
	case "message_plan":
		valid = !state.Source.Summary && state.Message == nil && state.Inputs != "" && state.Active != "" && state.Decoded == "" && state.Plan == "" && state.Generation == ""
	case "assembly", "completed":
		valid = state.Message == nil && state.Inputs != "" && state.Child == "" && state.Plan != "" && state.Source.Summary == (state.Decoded != "")
		if state.Phase == "assembly" {
			valid = valid && state.Active != "" && state.Generation == ""
		} else {
			valid = valid && state.Active == "" && state.Generation != ""
		}
	}
	if !valid {
		return ErrIntegrity
	}
	return nil
}

func (builder *docEventDisplayBuilder) startMessageInputs(discovery DocFieldDiscoveryState) error {
	source, err := discovery.MessageInputSource(DocRangeEntry{DocEventIndex: DocEventIndex{Hash: builder.state.Source.EventHash, Length: builder.state.Source.EventBytes}, Kind: EventMessage, CompactSummary: builder.state.Source.Summary})
	if err != nil {
		return err
	}
	state, err := NewDocMessageInputState(source)
	if err != nil {
		return err
	}
	builder.state.Active, err = builder.put(state.Encode(source))
	builder.state.Phase, builder.state.Message = "message_inputs", &source
	return err
}

func (builder *docEventDisplayBuilder) messageInputsStep(load func(context.Context, int, int) ([]byte, error)) error {
	source := *builder.state.Message
	raw, err := builder.read(builder.state.Active, MaxDocDisplayStateBytes)
	if err != nil {
		return err
	}
	state, err := DecodeDocMessageInputState(source, builder.state.Active, raw)
	if err != nil {
		return err
	}
	if !state.Done() {
		step, err := StepDocMessageInput(builder.ctx, source, state, load, builder.loadObject)
		if err != nil {
			return err
		}
		if err := builder.append(step.Objects); err != nil {
			return err
		}
		state = step.State
	}
	if !state.Done() {
		builder.state.Active, err = builder.put(state.Encode(source))
		return err
	}
	generation, objects, err := FinishDocMessageInput(builder.ctx, source, state)
	if err != nil {
		return err
	}
	if err := builder.append(objects); err != nil {
		return err
	}
	builder.state.Inputs, err = builder.put(generation.Encode())
	if err != nil {
		return err
	}
	builder.state.Message = nil
	if source.Summary {
		summary := DocSummaryTextSource{Version: 1, Profile: DocSummaryTextProfile, Message: generation}
		state, err := NewDocSummaryTextState(summary)
		if err != nil {
			return err
		}
		builder.state.Active, err = builder.put(state.Encode(summary))
		builder.state.Phase = "summary_text"
		return err
	}
	display := DocMessageDisplaySource{Version: 1, Profile: DocMessageDisplayProfile, Input: generation}
	stateDisplay, err := NewDocMessageDisplayState(display)
	if err != nil {
		return err
	}
	builder.state.Active, err = builder.put(stateDisplay.Encode(display))
	builder.state.Phase = "message_plan"
	return err
}

func (builder *docEventDisplayBuilder) messageInputs() (DocMessageInputGeneration, error) {
	raw, err := builder.read(builder.state.Inputs, MaxDocDisplaySourceBytes)
	if err != nil {
		return DocMessageInputGeneration{}, err
	}
	result, err := DecodeDocMessageInputGeneration(builder.state.Inputs, raw)
	if err != nil || result.Source.EventHash != builder.state.Source.EventHash || result.Source.EventBytes != builder.state.Source.EventBytes || result.Source.Summary != builder.state.Source.Summary {
		return DocMessageInputGeneration{}, ErrIntegrity
	}
	return result, nil
}

func (builder *docEventDisplayBuilder) assemblyInput() (DocDisplayAssemblyInput, error) {
	if builder.state.ToolEvent != "" {
		return builder.toolEventAssemblyInput()
	}
	if builder.state.Source.Kind == EventToolResult {
		return builder.toolResultAssemblyInput()
	}
	if builder.state.Source.Kind == EventReasoning {
		plan, err := builder.plan()
		if err != nil {
			return DocDisplayAssemblyInput{}, err
		}
		return NewDocDecodedMarkdownAssemblyInput(plan)
	}
	inputs, err := builder.messageInputs()
	if err != nil {
		return DocDisplayAssemblyInput{}, err
	}
	raw, err := builder.read(builder.state.Plan, MaxDocDisplaySourceBytes)
	if err != nil {
		return DocDisplayAssemblyInput{}, err
	}
	if builder.state.Source.Summary {
		decoded, err := builder.summaryDecoded(inputs)
		if err != nil {
			return DocDisplayAssemblyInput{}, err
		}
		plan, err := DecodeDocSummaryMarkdownPlan(builder.state.Plan, raw)
		if err != nil || plan.Input != decoded {
			return DocDisplayAssemblyInput{}, ErrIntegrity
		}
		return NewDocSummaryMarkdownAssemblyInput(plan)
	}
	plan, err := DecodeDocMessageDisplayPlan(builder.state.Plan, raw)
	if err != nil || plan.Source.Input != inputs {
		return DocDisplayAssemblyInput{}, ErrIntegrity
	}
	return NewDocMessageDisplayAssemblyInput(plan)
}
