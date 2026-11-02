package domain

import "context"

func (builder *docEventDisplayBuilder) summaryText(load func(context.Context, int, int) ([]byte, error)) error {
	inputs, err := builder.messageInputs()
	if err != nil {
		return err
	}
	source := DocSummaryTextSource{Version: 1, Profile: DocSummaryTextProfile, Message: inputs}
	raw, err := builder.read(builder.state.Active, MaxDocDisplayStateBytes)
	if err != nil {
		return err
	}
	state, err := DecodeDocSummaryTextState(source, builder.state.Active, raw)
	if err != nil {
		return err
	}
	if !state.Done {
		step, err := StepDocSummaryText(builder.ctx, source, state, load, builder.loadObject)
		if err != nil {
			return err
		}
		if err := builder.append(step.Objects); err != nil {
			return err
		}
		state = step.State
	}
	if !state.Done {
		builder.state.Active, err = builder.put(state.Encode(source))
		return err
	}
	generation, objects, err := FinishDocSummaryText(builder.ctx, source, state, builder.loadObject)
	if err != nil {
		return err
	}
	if err := builder.append(objects); err != nil {
		return err
	}
	builder.state.Decoded, err = builder.put(generation.Encode())
	if err != nil {
		return err
	}
	markdown, err := NewDocMarkdownPlanState(DocMarkdownPlanInput{Hash: generation.Hash, Bytes: generation.TextBytes})
	if err != nil {
		return err
	}
	builder.state.Active, err = builder.put(markdown.Encode())
	builder.state.Phase = "markdown"
	return err
}

func (builder *docEventDisplayBuilder) summaryDecoded(inputs DocMessageInputGeneration) (DocSummaryTextGeneration, error) {
	raw, err := builder.read(builder.state.Decoded, MaxDocDisplaySourceBytes)
	if err != nil {
		return DocSummaryTextGeneration{}, err
	}
	result, err := DecodeDocSummaryTextGeneration(builder.state.Decoded, raw)
	if err != nil || result.Source.Message != inputs {
		return DocSummaryTextGeneration{}, ErrIntegrity
	}
	return result, nil
}

func (builder *docEventDisplayBuilder) summaryMarkdown() error {
	inputs, err := builder.messageInputs()
	if err != nil {
		return err
	}
	decoded, err := builder.summaryDecoded(inputs)
	if err != nil {
		return err
	}
	return builder.compileMarkdown(DocMarkdownPlanInput{Hash: decoded.Hash, Bytes: decoded.TextBytes}, func(state DocMarkdownPlanState) (DocMarkdownPlanStep, error) {
		return StepDocSummaryMarkdownPlan(builder.ctx, decoded, state, builder.loadObject)
	}, func(markdown DocMarkdownPlan) ([]byte, error) {
		return (DocSummaryMarkdownPlan{Version: 1, Input: decoded, Markdown: markdown}).Encode()
	})
}
