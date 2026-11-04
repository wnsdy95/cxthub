package domain

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"unicode/utf8"
)

var errDocToolValueYield = errors.New("tool value step budget reached")

type DocToolValueStep struct {
	prior     ContentHash
	state     DocToolValueState
	objects   []DocRangeObject
	readBytes int
	actions   int
	frames    int
}

func (step DocToolValueStep) Prior() ContentHash       { return step.prior }
func (step DocToolValueStep) ReadBytes() int           { return step.readBytes }
func (step DocToolValueStep) Actions() int             { return step.actions }
func (step DocToolValueStep) FrameOperations() int     { return step.frames }
func (step DocToolValueStep) State() DocToolValueState { return cloneDocToolValueState(step.state) }
func (step DocToolValueStep) Objects() []DocRangeObject {
	objects := make([]DocRangeObject, len(step.objects))
	for index, object := range step.objects {
		objects[index] = DocRangeObject{Hash: object.Hash, Bytes: bytes.Clone(object.Bytes)}
	}
	return objects
}

func (builder *docToolValueBuilder) completeValue(node DocToolTreeNode) error {
	builder.state.Value, builder.state.Active, builder.state.Start = nil, "", 0
	if builder.state.Frame == nil {
		if !sameDocToolValueNode(builder.source.Tree, node, builder.source.Root) {
			return ErrIntegrity
		}
		builder.state.Phase = "complete"
		return nil
	}
	frame := builder.state.Frame
	if frame.Child == nil || !sameDocToolValueNode(builder.source.Tree, *frame.Child, node) {
		return ErrIntegrity
	}
	frame.Previous, frame.Next, frame.Child = node.Value.To, frame.Next+1, nil
	builder.state.Phase = "next"
	return nil
}

func (builder *docToolValueBuilder) enter(loadOrder func(context.Context, int) (DocToolOrderPlan, error)) error {
	node := *builder.state.Value
	if builder.frames == DocToolTreeStepFrames {
		return errDocToolValueYield
	}
	plan, err := loadOrder(builder.buffer.ctx, node.Value.From)
	if err != nil {
		return err
	}
	raw, err := plan.Encode(builder.source.Tree, builder.source.Keys)
	if err != nil || !sameDocToolValueNode(builder.source.Tree, node, plan.Source.Parent) {
		return ErrIntegrity
	}
	frame := docToolValueFrame{Version: 1, Source: builder.state.Source, Node: cloneDocToolTreeNode(node), Plan: HashContent(raw)}
	if _, err := builder.framePlan(frame); err != nil {
		return err
	}
	if builder.state.Frame != nil {
		encoded, err := builder.state.Frame.encode(builder.source)
		if err != nil {
			return err
		}
		frame.Parent, err = builder.buffer.put(encoded, nil)
		if err != nil {
			return err
		}
		builder.frames++
	}
	if err := builder.emit(string(byte(node.Type)), DocFindSpan{From: node.Value.From, To: node.Value.From + 1}, true); err != nil {
		return err
	}
	builder.state.Frame, builder.state.Value, builder.state.Phase = &frame, nil, "next"
	return nil
}

func (builder *docToolValueBuilder) next() error {
	frame := builder.state.Frame
	plan, err := builder.framePlan(*frame)
	if err != nil {
		return err
	}
	if frame.Next == plan.Source.Count {
		if frame.Parent != "" && builder.frames == DocToolTreeStepFrames {
			return errDocToolValueYield
		}
		anchor := DocFindSpan{From: frame.Node.Value.To - 1, To: frame.Node.Value.To}
		if frame.Next > 0 {
			if err := builder.emit("\n"+strings.Repeat("  ", frame.Node.Depth), anchor, false); err != nil {
				return err
			}
		}
		closing := "]"
		if frame.Node.Type == DocToolValueObject {
			closing = "}"
		}
		if err := builder.emit(closing, anchor, true); err != nil {
			return err
		}
		node, parent := frame.Node, frame.Parent
		builder.state.Frame = nil
		if parent != "" {
			builder.frames++
			raw, err := builder.buffer.read(parent, MaxDocDisplaySourceBytes)
			if err != nil {
				return err
			}
			previous, err := decodeDocToolValueFrame(builder.source, parent, raw)
			if err != nil || previous.Child == nil || !sameDocToolValueNode(builder.source.Tree, *previous.Child, node) {
				return ErrIntegrity
			}
			if err := builder.validateFrame(previous); err != nil {
				return err
			}
			builder.state.Frame = &previous
		}
		return builder.completeValue(node)
	}
	child, err := builder.entry(*frame, frame.Next)
	if err != nil {
		return err
	}
	anchor := DocFindSpan{From: frame.Node.Value.From, To: frame.Node.Value.From + 1}
	if frame.Next > 0 {
		raw, err := builder.raw(builder.buffer.ctx, frame.Previous, 1)
		if err != nil {
			return err
		}
		if raw[0] == ',' {
			anchor = DocFindSpan{From: frame.Previous, To: frame.Previous + 1}
		}
		if err := builder.emit(",", anchor, false); err != nil {
			return err
		}
	}
	if err := builder.emit("\n"+strings.Repeat("  ", frame.Node.Depth+1), anchor, false); err != nil {
		return err
	}
	node := cloneDocToolTreeNode(child.Node)
	frame.Child = &node
	if frame.Node.Type == DocToolValueObject {
		builder.state.Phase = "key"
	} else {
		value := cloneDocToolTreeNode(node)
		builder.state.Phase, builder.state.Value = "value", &value
	}
	return nil
}

func (builder *docToolValueBuilder) stringSource() DocToolStringSource {
	span := DocFindSpan{}
	if builder.state.Phase == "key" {
		span = *builder.state.Frame.Child.Key
	} else {
		span = builder.state.Value.Value
	}
	return DocToolStringSource{Version: 1, Profile: DocToolStringProfile, EventHash: builder.source.Tree.EventHash, EventBytes: builder.source.Tree.EventBytes, Token: span, Mode: DocToolStringQuoted}
}

func (builder *docToolValueBuilder) quoted() error {
	source := builder.stringSource()
	state, err := NewDocToolStringState(source)
	if err != nil {
		return err
	}
	if builder.state.Active != "" {
		raw, err := builder.buffer.read(builder.state.Active, MaxDocDisplayStateBytes)
		if err != nil {
			return err
		}
		state, err = DecodeDocToolStringState(source, builder.state.Active, raw)
		if err != nil {
			return err
		}
	}
	if min(DocDisplayStepSourceBytes, source.Token.To-state.Next) > DocToolValueStepSourceBytes-builder.read {
		return errDocToolValueYield
	}
	step, err := StepDocToolString(builder.buffer.ctx, source, state, builder.raw, builder.buffer.loadObject)
	if err != nil {
		return err
	}
	if err := builder.fragment(step.page.Display); err != nil {
		return err
	}
	if step.State.Done {
		builder.state.Active, builder.state.Start = "", 0
		if builder.state.Phase == "key" {
			builder.state.Phase, builder.state.Offset = "colon", source.Token.To
			return nil
		}
		return builder.completeValue(*builder.state.Value)
	}
	if builder.state.Active == "" {
		builder.state.Start = builder.state.TextBytes - step.State.TextBytes
	}
	if err := builder.buffer.append(step.Objects); err != nil {
		return err
	}
	builder.state.Active, err = builder.buffer.put(step.State.Encode(source))
	return err
}

func (builder *docToolValueBuilder) number() error {
	source, err := NewDocToolNumberSource(builder.source.Tree, *builder.state.Value)
	if err != nil {
		return err
	}
	state, err := NewDocToolNumberState(source)
	if err != nil {
		return err
	}
	if builder.state.Active != "" {
		raw, err := builder.buffer.read(builder.state.Active, MaxDocDisplayStateBytes)
		if err != nil {
			return err
		}
		state, err = DecodeDocToolNumberState(source, builder.state.Active, raw)
		if err != nil {
			return err
		}
	}
	if min(DocToolNumberStepSourceBytes, source.Node.Value.To-state.Next) > DocToolValueStepSourceBytes-builder.read {
		return errDocToolValueYield
	}
	step, err := StepDocToolNumber(builder.buffer.ctx, source, state, builder.raw)
	if err != nil {
		return err
	}
	if step.State().Done() {
		plan, err := FinishDocToolNumber(builder.buffer.ctx, source, step.State())
		if err != nil {
			return err
		}
		if err := builder.emit(plan.Text, source.Node.Value, plan.Linear); err != nil {
			return err
		}
		return builder.completeValue(source.Node)
	}
	builder.state.Start = builder.state.TextBytes
	builder.state.Active, err = builder.buffer.put(step.State().Encode(source))
	return err
}

func (builder *docToolValueBuilder) colon() error {
	end := builder.state.Frame.Child.Value.From
	for builder.state.Offset < end && builder.read < DocToolValueStepSourceBytes {
		length := min(4096, end-builder.state.Offset, DocToolValueStepSourceBytes-builder.read)
		raw, err := builder.raw(builder.buffer.ctx, builder.state.Offset, length)
		if err != nil {
			return err
		}
		for index, character := range raw {
			if character == ' ' || character == '\t' || character == '\r' || character == '\n' {
				continue
			}
			if character != ':' {
				return ErrIntegrity
			}
			from := builder.state.Offset + index
			if err := builder.emit(": ", DocFindSpan{From: from, To: from + 1}, false); err != nil {
				return err
			}
			value := cloneDocToolTreeNode(*builder.state.Frame.Child)
			builder.state.Value, builder.state.Phase, builder.state.Offset = &value, "value", 0
			return nil
		}
		builder.state.Offset += length
	}
	if builder.state.Offset == end {
		return ErrIntegrity
	}
	return errDocToolValueYield
}

func StepDocToolValue(ctx context.Context, source DocToolValueSource, prior DocToolValueState, loadRaw func(context.Context, int, int) ([]byte, error), loadObject func(context.Context, ContentHash) ([]byte, error), loadOrder func(context.Context, int) (DocToolOrderPlan, error)) (result DocToolValueStep, failure error) {
	defer func() {
		if err := ctx.Err(); err != nil {
			failure = err
		}
		if failure != nil {
			result = DocToolValueStep{}
		}
	}()
	raw, err := prior.Encode(source)
	if err != nil || prior.Done() || loadRaw == nil || loadObject == nil || loadOrder == nil {
		return result, ErrIntegrity
	}
	state := cloneDocToolValueState(prior)
	builder := docToolValueBuilder{source: source, state: state, loadRaw: loadRaw, buffer: docEventDisplayBuilder{ctx: ctx, load: newDocToolValueLoader(loadObject)}}
	if err := builder.checkpoint(); err != nil {
		return result, err
	}
	for builder.actions < DocToolValueStepActions && builder.emitted <= DocToolValueStepOutputBytes-DocDisplayPageTextBytes && len(builder.buffer.objects) < MaxDocEventDisplayStepObjects/2 && !builder.state.Done() {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		switch builder.state.Phase {
		case "next":
			err = builder.next()
		case "key", "string":
			err = builder.quoted()
		case "number":
			err = builder.number()
		case "colon":
			err = builder.colon()
		case "value":
			switch builder.state.Value.Type {
			case DocToolValueObject, DocToolValueArray:
				err = builder.enter(loadOrder)
			case DocToolValueString:
				builder.state.Phase = "string"
				err = builder.quoted()
				if errors.Is(err, errDocToolValueYield) && builder.state.Active == "" {
					builder.state.Phase = "value"
				}
			case DocToolValueNumber:
				builder.state.Phase = "number"
				err = builder.number()
				if errors.Is(err, errDocToolValueYield) && builder.state.Active == "" {
					builder.state.Phase = "value"
				}
			default:
				node := *builder.state.Value
				text := "null"
				if node.Type == DocToolValueTrue {
					text = "true"
				} else if node.Type == DocToolValueFalse {
					text = "false"
				}
				err = builder.emit(text, node.Value, true)
				if err == nil {
					err = builder.completeValue(node)
				}
			}
		default:
			err = ErrIntegrity
		}
		if errors.Is(err, errDocToolValueYield) {
			break
		}
		if err != nil {
			return result, err
		}
		builder.actions++
	}
	if builder.state.Done() {
		if err := builder.seal(true); err != nil {
			return result, err
		}
	} else {
		if builder.text.Len() > 0 && utf8.RuneCountInString(builder.text.String()) >= DocToolValuePageMinRunes {
			if err := builder.seal(false); err != nil {
				return result, err
			}
		} else if err := builder.savePending(); err != nil {
			return result, err
		}
	}
	if err := builder.recordReceipt(); err != nil {
		return result, err
	}
	encoded, err := builder.state.Encode(source)
	if err != nil || HashContent(encoded) == HashContent(raw) {
		return result, ErrIntegrity
	}
	return DocToolValueStep{prior: HashContent(raw), state: builder.state, objects: builder.buffer.objects, readBytes: builder.read, actions: builder.actions, frames: builder.frames}, nil
}

func FinishDocToolValue(ctx context.Context, source DocToolValueSource, state DocToolValueState, load func(context.Context, ContentHash) ([]byte, error)) (plan DocToolValuePlan, objects []DocRangeObject, failure error) {
	defer func() {
		if err := ctx.Err(); err != nil {
			failure = err
		}
		if failure != nil {
			plan, objects = DocToolValuePlan{}, nil
		}
	}()
	if !state.Done() || load == nil {
		return plan, nil, ErrIntegrity
	}
	builder := docToolValueBuilder{source: source, state: cloneDocToolValueState(state), buffer: docEventDisplayBuilder{ctx: ctx, load: newDocToolValueLoader(load)}}
	if err := builder.checkpoint(); err != nil {
		return plan, nil, err
	}
	root, objects, err := state.Directory.Finish(ctx)
	if err != nil {
		return plan, nil, err
	}
	plan = DocToolValuePlan{Version: 1, Source: source, Pages: root, TextBytes: state.TextBytes}
	return plan, objects, plan.Validate()
}
