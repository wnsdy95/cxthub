package domain

import (
	"context"
	"errors"
)

var errDocEventDisplayBatchYield = errors.New("message preparation batch must yield")

type docEventDisplayBuilder struct {
	ctx         context.Context
	state       DocEventDisplayPreparation
	load        func(context.Context, ContentHash) ([]byte, error)
	objects     []DocRangeObject
	bytes       int
	yieldBudget bool
	tool        *DocToolTreeScanStep
	keys        *DocToolKeyMapStep
	orders      *DocToolOrdersStep
}

func (builder *docEventDisplayBuilder) read(hash ContentHash, limit int) ([]byte, error) {
	if err := builder.ctx.Err(); err != nil {
		return nil, err
	}
	if ValidateContentHash(hash) != nil || builder.load == nil {
		return nil, ErrIntegrity
	}
	for _, object := range builder.objects {
		if object.Hash == hash {
			if len(object.Bytes) > limit {
				return nil, ErrIntegrity
			}
			return object.Bytes, nil
		}
	}
	raw, err := builder.load(builder.ctx, hash)
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 || len(raw) > limit || HashContent(raw) != hash {
		return nil, ErrIntegrity
	}
	return raw, builder.ctx.Err()
}

func (builder *docEventDisplayBuilder) loadObject(ctx context.Context, hash ContentHash) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return builder.read(hash, MaxDocDisplayPageBytes)
}

func (builder *docEventDisplayBuilder) put(raw []byte, failure error) (ContentHash, error) {
	if failure != nil {
		return "", failure
	}
	hash := HashContent(raw)
	for _, object := range builder.objects {
		if object.Hash == hash {
			return hash, nil
		}
	}
	if len(raw) == 0 || len(raw) > MaxDocDisplayPageBytes {
		return "", ErrIntegrity
	}
	if len(builder.objects) >= MaxDocEventDisplayStepObjects || len(raw) > MaxDocEventDisplayStepBytes-builder.bytes {
		if builder.yieldBudget {
			return "", errDocEventDisplayBatchYield
		}
		return "", ErrIntegrity
	}
	builder.objects = append(builder.objects, DocRangeObject{Hash: hash, Bytes: raw})
	builder.bytes += len(raw)
	return hash, nil
}

func (builder *docEventDisplayBuilder) append(objects []DocRangeObject) error {
	for _, object := range objects {
		if HashContent(object.Bytes) != object.Hash {
			return ErrIntegrity
		}
		if _, err := builder.put(object.Bytes, nil); err != nil {
			return err
		}
	}
	return nil
}

func StepDocEventDisplay(ctx context.Context, prior DocEventDisplayPreparation, loadRaw func(context.Context, int, int) ([]byte, error), loadObject func(context.Context, ContentHash) ([]byte, error), preparation ...DocToolPreparationReader) (result DocEventDisplayStep, failure error) {
	defer func() {
		if err := ctx.Err(); err != nil {
			failure = err
		}
		if failure != nil {
			result = DocEventDisplayStep{}
		}
	}()
	if err := ctx.Err(); err != nil {
		return result, err
	}
	raw, err := prior.Encode()
	if err != nil || prior.Phase == "completed" || loadObject == nil || loadRaw == nil || len(preparation) > 1 {
		return result, ErrIntegrity
	}
	cloned, err := DecodeDocEventDisplayPreparation(HashContent(raw), raw)
	if err != nil {
		return result, err
	}
	builder := docEventDisplayBuilder{ctx: ctx, state: cloned, load: loadObject}
	var tools DocToolPreparationReader
	if len(preparation) == 1 {
		tools = preparation[0]
	}
	switch prior.Phase {
	case "tool_discovery":
		err = builder.toolDiscover(loadRaw)
	case "tool_tree":
		err = builder.toolTreeScan(loadRaw)
	case "tool_tree_done", "tool_keys":
		if tools.Nodes == nil {
			err = ErrDocFindDisplayUnavailable
		} else if prior.Phase == "tool_tree_done" {
			err = builder.toolKeysInitialize()
		} else {
			err = builder.toolKeysStep(loadRaw, tools.Nodes)
		}
	case "tool_keys_done", "tool_orders":
		if tools.Nodes == nil || tools.OrderSource == nil || tools.OrderEntries == nil {
			err = ErrDocFindDisplayUnavailable
		} else if prior.Phase == "tool_keys_done" {
			err = builder.toolOrdersInitialize()
		} else {
			err = builder.toolOrdersStep(tools)
		}
	case "tool_orders_done", "tool_values":
		if tools.OrderPlan == nil || tools.Nodes == nil {
			err = ErrDocFindDisplayUnavailable
		} else if prior.Phase == "tool_orders_done" {
			if prior.Source.Kind == EventToolCall || prior.ToolTree.Type == DocToolValueString || prior.ToolTree.Type == DocToolValueNull {
				err = builder.toolPolicyInitialize(tools)
			} else {
				err = builder.toolValuesInitialize(tools)
			}
		} else {
			err = builder.toolValuesStep(loadRaw, tools.OrderPlan)
		}
	case "tool_values_done":
		err = builder.toolResultAssembly()
	case "tool_assembly":
		err = builder.assemble()
	case "tool_empty":
		err = builder.toolPolicyInitialize(tools)
	case "tool_policy":
		err = builder.toolPolicyStep(loadRaw, tools)
	case "tool_event_assembly":
		err = builder.assemble()
	case "discovery":
		err = builder.discover(loadRaw)
	case "decoding":
		err = builder.decode(loadRaw)
	case "message_inputs":
		err = builder.messageInputsStep(loadRaw)
	case "summary_text":
		err = builder.summaryText(loadRaw)
	case "message_plan":
		err = builder.messageBatch(loadRaw)
	case "markdown":
		if prior.Source.Kind == EventMessage {
			err = builder.summaryMarkdown()
		} else {
			err = builder.markdown()
		}
	case "assembly":
		err = builder.assemble()
	default:
		err = ErrIntegrity
	}
	if err != nil {
		return result, err
	}
	encoded, err := builder.state.Encode()
	if err != nil || HashContent(encoded) == HashContent(raw) {
		return result, ErrIntegrity
	}
	return DocEventDisplayStep{prior: HashContent(raw), state: builder.state, objects: builder.objects, tool: builder.tool, keys: builder.keys, orders: builder.orders}, nil
}

func (builder *docEventDisplayBuilder) discover(load func(context.Context, int, int) ([]byte, error)) error {
	state, err := NewDocFieldDiscoveryState(builder.state.Source.EventHash, builder.state.Source.EventBytes)
	if err != nil {
		return err
	}
	if builder.state.Active != "" {
		raw, err := builder.read(builder.state.Active, MaxDocFieldDiscoveryStateBytes)
		if err != nil {
			return err
		}
		state, err = DecodeDocFieldDiscoveryState(builder.state.Active, raw)
		if err != nil || state.EventHash != builder.state.Source.EventHash || state.EventBytes != builder.state.Source.EventBytes || state.Done {
			return ErrIntegrity
		}
	}
	length := min(DocFieldDiscoveryStepBytes, state.EventBytes-state.Next)
	raw, err := load(builder.ctx, state.Next, length)
	if err != nil {
		return err
	}
	if len(raw) != length {
		return ErrIntegrity
	}
	step, err := AdvanceDocFieldDiscovery(builder.ctx, state, raw)
	if err != nil {
		return err
	}
	state, err = step.State()
	if err != nil {
		return err
	}
	if !state.Done {
		builder.state.Active, err = builder.put(state.Encode())
		return err
	}
	if state.Kind != builder.state.Source.Kind {
		return ErrIntegrity
	}
	if _, err := state.FindFields(); err != nil {
		return err
	}
	if state.Kind == EventMessage {
		return builder.startMessageInputs(state)
	}
	if builder.state.Source.EmptyReasoning {
		builder.state.Plan, err = builder.put(state.Encode())
		if err != nil {
			return err
		}
		source := DocDisplayProjectionSource{Version: 1, Profile: DocDisplayProjectionProfile, EventHash: state.EventHash, EventBytes: state.EventBytes, Plan: builder.state.Plan}
		output, err := NewDocDisplayProjectionState(source)
		if err != nil {
			return err
		}
		output.Done = true
		generation, objects, err := FinishDocDisplayProjection(builder.ctx, source, output)
		if err != nil {
			return err
		}
		if err := builder.append(objects); err != nil {
			return err
		}
		builder.state.Generation, err = builder.put(generation.Encode())
		builder.state.Phase, builder.state.Active = "completed", ""
		return err
	}
	source, err := state.ReasoningTextSource()
	if err != nil {
		return err
	}
	decoded, err := NewDocDecodedTextState(source)
	if err != nil {
		return err
	}
	builder.state.Active, err = builder.put(decoded.Encode(source))
	builder.state.Phase, builder.state.Input = "decoding", &source
	return err
}

func (builder *docEventDisplayBuilder) decode(load func(context.Context, int, int) ([]byte, error)) error {
	source := *builder.state.Input
	raw, err := builder.read(builder.state.Active, MaxDocDisplayStateBytes)
	if err != nil {
		return err
	}
	state, err := DecodeDocDecodedTextState(source, builder.state.Active, raw)
	if err != nil {
		return err
	}
	step, err := StepDocDecodedText(builder.ctx, source, state, load, builder.loadObject)
	if err != nil {
		return err
	}
	if err := builder.append(step.Objects); err != nil {
		return err
	}
	if !step.State.Done {
		builder.state.Active, err = builder.put(step.State.Encode(source))
		return err
	}
	generation, objects, err := FinishDocDecodedText(builder.ctx, source, step.State)
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
	builder.state.Phase, builder.state.Input = "markdown", nil
	return err
}

func (builder *docEventDisplayBuilder) decoded() (DocDecodedTextGeneration, error) {
	raw, err := builder.read(builder.state.Decoded, MaxDocDisplaySourceBytes)
	if err != nil {
		return DocDecodedTextGeneration{}, err
	}
	result, err := DecodeDocDecodedTextGeneration(builder.state.Decoded, raw)
	if err != nil || result.Source.EventHash != builder.state.Source.EventHash || result.Source.EventBytes != builder.state.Source.EventBytes {
		return DocDecodedTextGeneration{}, ErrIntegrity
	}
	return result, nil
}

func (builder *docEventDisplayBuilder) markdown() error {
	decoded, err := builder.decoded()
	if err != nil {
		return err
	}
	return builder.compileMarkdown(DocMarkdownPlanInput{Hash: decoded.Hash, Bytes: decoded.TextBytes}, func(state DocMarkdownPlanState) (DocMarkdownPlanStep, error) {
		return StepDocDecodedMarkdownPlan(builder.ctx, decoded, state, builder.loadObject)
	}, func(markdown DocMarkdownPlan) ([]byte, error) {
		return (DocDecodedMarkdownPlan{Version: 1, Input: decoded, Markdown: markdown}).Encode()
	})
}

func (builder *docEventDisplayBuilder) compileMarkdown(input DocMarkdownPlanInput, advance func(DocMarkdownPlanState) (DocMarkdownPlanStep, error), encode func(DocMarkdownPlan) ([]byte, error)) error {
	raw, err := builder.read(builder.state.Active, MaxDocDisplayStateBytes)
	if err != nil {
		return err
	}
	state, err := DecodeDocMarkdownPlanState(builder.state.Active, raw)
	if err != nil || state.Input != input {
		return ErrIntegrity
	}
	step, err := advance(state)
	if err != nil {
		return err
	}
	if err := builder.append(step.Objects); err != nil {
		return err
	}
	if !step.State.Done() {
		builder.state.Active, err = builder.put(step.State.Encode())
		return err
	}
	markdown, objects, err := FinishDocMarkdownPlan(builder.ctx, step.State)
	if err != nil {
		return err
	}
	if err := builder.append(objects); err != nil {
		return err
	}
	builder.state.Plan, err = builder.put(encode(markdown))
	if err != nil {
		return err
	}
	return builder.beginAssembly()
}

func (builder *docEventDisplayBuilder) beginAssembly() error {
	input, err := builder.assemblyInput()
	if err != nil {
		return err
	}
	assembly, err := NewDocDisplayAssemblyState(input)
	if err != nil {
		return err
	}
	builder.state.Active, err = builder.put(assembly.Encode(input))
	builder.state.Phase = "assembly"
	return err
}

func (builder *docEventDisplayBuilder) plan() (DocDecodedMarkdownPlan, error) {
	decoded, err := builder.decoded()
	if err != nil {
		return DocDecodedMarkdownPlan{}, err
	}
	raw, err := builder.read(builder.state.Plan, MaxDocDisplaySourceBytes)
	if err != nil {
		return DocDecodedMarkdownPlan{}, err
	}
	plan, err := DecodeDocDecodedMarkdownPlan(builder.state.Plan, raw)
	if err != nil || plan.Input != decoded {
		return DocDecodedMarkdownPlan{}, ErrIntegrity
	}
	return plan, nil
}

func (builder *docEventDisplayBuilder) assemble() error {
	input, err := builder.assemblyInput()
	if err != nil {
		return err
	}
	raw, err := builder.read(builder.state.Active, MaxDocDisplayStateBytes)
	if err != nil {
		return err
	}
	state, err := DecodeDocDisplayAssemblyState(input, builder.state.Active, raw)
	if err != nil {
		return err
	}
	step := DocDisplayAssemblyStep{State: state}
	if !state.Output.Done {
		step, err = StepDocDisplayAssembly(builder.ctx, input, state, builder.loadObject)
		if err != nil {
			return err
		}
		if err := builder.append(step.Objects); err != nil {
			return err
		}
	}
	if !step.State.Output.Done {
		builder.state.Active, err = builder.put(step.State.Encode(input))
		return err
	}
	generation, objects, err := FinishDocDisplayAssembly(builder.ctx, input, step.State, builder.loadObject)
	if err != nil {
		return err
	}
	if err := builder.append(objects); err != nil {
		return err
	}
	builder.state.Generation, err = builder.put(generation.Encode())
	builder.state.Phase, builder.state.Active = "completed", ""
	return err
}
