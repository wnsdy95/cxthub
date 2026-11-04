package domain

import (
	"context"
	"encoding/json"
	"strings"
	"unicode/utf8"
)

const docToolKeyMapInlineTokenBytes = 128

type docToolKeyMapBuilder struct {
	buffer docEventDisplayBuilder
	tree   DocToolTreeSource
	source DocToolKeyMapSource
	state  DocToolKeyMapState
	inputs []DocToolTreeNode
	items  []DocToolKeyItem
	read   int
}

func (builder *docToolKeyMapBuilder) keySource() DocToolStringSource {
	return DocToolStringSource{Version: 1, Profile: DocToolStringProfile, EventHash: builder.tree.EventHash,
		EventBytes: builder.tree.EventBytes, Token: *builder.state.Current.Key, Mode: DocToolStringQuoted}
}

func (builder *docToolKeyMapBuilder) finishKey(digest ContentHash, isIndex bool, index uint32) error {
	item := DocToolKeyItem{Node: cloneDocToolTreeNode(*builder.state.Current), Digest: digest, IsArrayIndex: isIndex, ArrayIndex: index}
	if item.Validate(builder.tree) != nil {
		return ErrIntegrity
	}
	builder.items = append(builder.items, item)
	builder.state.After, builder.state.Seen, builder.state.Keys = item.Node.Value.From, builder.state.Seen+1, builder.state.Keys+1
	builder.state.Current, builder.state.Active, builder.state.Plan, builder.state.Phase = nil, "", "", "scan"
	return nil
}

func (builder *docToolKeyMapBuilder) shortKey(raw []byte) error {
	if len(raw) > docToolKeyMapInlineTokenBytes || len(raw) < 2 || raw[0] != '"' || raw[len(raw)-1] != '"' || !utf8.Valid(raw) || !json.Valid(raw) {
		return ErrIntegrity
	}
	var quoted strings.Builder
	quoted.WriteByte('"')
	for position := 1; position < len(raw)-1; {
		value, width, _, err := docToolStringRune(raw[position:], DocToolStringQuoted)
		if err != nil || position+width > len(raw)-1 {
			return ErrIntegrity
		}
		quoted.WriteString(value)
		position += width
	}
	quoted.WriteByte('"')
	text := quoted.String()
	possible, digits, index := advanceDocToolKeyArrayIndex(true, 0, 0, text[1:len(text)-1])
	return builder.finishKey(HashContent([]byte(text)), possible && digits > 0, index)
}

func (builder *docToolKeyMapBuilder) stringStep(load func(context.Context, int, int) ([]byte, error)) error {
	source := builder.keySource()
	raw, err := builder.buffer.read(builder.state.Active, MaxDocDisplayStateBytes)
	if err != nil {
		return err
	}
	state, err := DecodeDocToolStringState(source, builder.state.Active, raw)
	if err != nil {
		return err
	}
	step, err := StepDocToolString(builder.buffer.ctx, source, state, load, builder.buffer.loadObject)
	if err != nil {
		return err
	}
	if err := builder.buffer.append(step.Objects); err != nil {
		return err
	}
	if !step.State.Done {
		builder.state.Active, err = builder.buffer.put(step.State.Encode(source))
		return err
	}
	plan, objects, err := FinishDocToolString(builder.buffer.ctx, source, step.State, builder.buffer.loadObject)
	if err != nil {
		return err
	}
	if err := builder.buffer.append(objects); err != nil {
		return err
	}
	builder.state.Plan, err = builder.buffer.put(plan.Encode())
	if err != nil {
		return err
	}
	digestSource, err := NewDocToolKeyDigestSource(plan)
	if err != nil {
		return err
	}
	digestState, err := NewDocToolKeyDigestState(digestSource)
	if err != nil {
		return err
	}
	builder.state.Active, err = builder.buffer.put(digestState.Encode(digestSource))
	builder.state.Phase = "digest"
	return err
}

func (builder *docToolKeyMapBuilder) digestStep() error {
	raw, err := builder.buffer.read(builder.state.Plan, MaxDocDisplaySourceBytes)
	if err != nil {
		return err
	}
	plan, err := DecodeDocToolStringPlan(builder.state.Plan, raw)
	if err != nil || plan.Source != builder.keySource() {
		return ErrIntegrity
	}
	source, err := NewDocToolKeyDigestSource(plan)
	if err != nil {
		return err
	}
	raw, err = builder.buffer.read(builder.state.Active, MaxDocToolKeyDigestStateBytes)
	if err != nil {
		return err
	}
	state, err := DecodeDocToolKeyDigestState(source, builder.state.Active, raw)
	if err != nil {
		return err
	}
	step, err := StepDocToolKeyDigest(builder.buffer.ctx, source, state, builder.buffer.loadObject)
	if err != nil {
		return err
	}
	if !step.State.Done {
		builder.state.Active, err = builder.buffer.put(step.State.Encode(source))
		return err
	}
	result, err := FinishDocToolKeyDigest(builder.buffer.ctx, source, step.State, builder.buffer.loadObject)
	if err != nil {
		return err
	}
	return builder.finishKey(result.Digest, result.IsArrayIndex, result.ArrayIndex)
}

func StepDocToolKeyMap(ctx context.Context, tree DocToolTreeSource, source DocToolKeyMapSource, prior DocToolKeyMapState, loadRaw func(context.Context, int, int) ([]byte, error), loadObject func(context.Context, ContentHash) ([]byte, error), loadNodes DocToolTreeNodeLoader) (result DocToolKeyMapStep, failure error) {
	defer func() {
		if err := ctx.Err(); err != nil {
			failure = err
		}
		if failure != nil {
			result = DocToolKeyMapStep{}
		}
	}()
	if err := ctx.Err(); err != nil {
		return result, err
	}
	encoded, err := prior.Encode(tree, source)
	if err != nil || prior.Done() || loadRaw == nil || loadObject == nil || loadNodes == nil {
		return result, ErrIntegrity
	}
	state, err := DecodeDocToolKeyMapState(tree, source, HashContent(encoded), encoded)
	if err != nil {
		return result, err
	}
	builder := docToolKeyMapBuilder{tree: tree, source: source, state: state, buffer: docEventDisplayBuilder{ctx: ctx, load: loadObject}}
	read := func(read context.Context, offset, length int) ([]byte, error) {
		if err := read.Err(); err != nil {
			return nil, err
		}
		if length <= 0 || length > DocFieldDiscoveryStepBytes-builder.read || offset < tree.Root.From || offset > tree.Root.To-length {
			return nil, ErrIntegrity
		}
		raw, err := loadRaw(read, offset, length)
		if err != nil {
			return nil, err
		}
		if len(raw) != length {
			return nil, ErrIntegrity
		}
		builder.read += length
		return raw, read.Err()
	}
	limit := DocToolKeyMapStepNodes
	if state.Current != nil {
		limit = 1
	}
	expectedNodes := min(limit, source.Nodes-prior.Seen)
	nodes, err := loadNodes(ctx, prior.After, expectedNodes)
	if err != nil {
		return result, err
	}
	if len(nodes) != expectedNodes {
		return result, ErrIntegrity
	}
	if state.Current != nil {
		stored, err := nodes[0].Encode(tree)
		expected, expectedErr := state.Current.Encode(tree)
		if err != nil || expectedErr != nil || HashContent(stored) != HashContent(expected) {
			return result, ErrIntegrity
		}
		builder.inputs = append(builder.inputs, cloneDocToolTreeNode(nodes[0]))
		if state.Phase == "string" {
			err = builder.stringStep(read)
		} else {
			err = builder.digestStep()
		}
		if err != nil {
			return result, err
		}
	} else {
		for _, node := range nodes {
			if err := ctx.Err(); err != nil {
				return result, err
			}
			if node.Validate(tree) != nil || node.Value.From <= builder.state.After || builder.state.Seen == 0 && node.Value.From != tree.Root.From {
				return result, ErrIntegrity
			}
			builder.inputs = append(builder.inputs, cloneDocToolTreeNode(node))
			if node.Key == nil {
				builder.state.After, builder.state.Seen = node.Value.From, builder.state.Seen+1
				continue
			}
			current := cloneDocToolTreeNode(node)
			builder.state.Current = &current
			if node.Key.To-node.Key.From <= docToolKeyMapInlineTokenBytes {
				raw, err := read(ctx, node.Key.From, node.Key.To-node.Key.From)
				if err != nil {
					return result, err
				}
				if err := builder.shortKey(raw); err != nil {
					return result, err
				}
				continue
			}
			inner, err := NewDocToolStringState(builder.keySource())
			if err != nil {
				return result, err
			}
			builder.state.Active, err = builder.buffer.put(inner.Encode(builder.keySource()))
			if err != nil {
				return result, err
			}
			builder.state.Phase = "string"
			break
		}
	}
	if builder.state.Seen == source.Nodes {
		builder.state.Phase = "done"
	}
	next, err := builder.state.Encode(tree, source)
	if err != nil || HashContent(next) == HashContent(encoded) {
		return result, ErrIntegrity
	}
	return DocToolKeyMapStep{source: source, prior: HashContent(encoded), state: builder.state, inputs: builder.inputs,
		items: builder.items, objects: builder.buffer.objects, readBytes: builder.read}, nil
}
