package domain

import (
	"bytes"
	"context"
)

type docToolOrdersBuilder struct {
	buffer docEventDisplayBuilder
	tree   DocToolTreeSource
	keys   DocToolKeyMapSource
	source DocToolOrdersSource
	state  DocToolOrdersState
	inputs []DocToolTreeNode
	order  *DocToolOrderStep
	plan   *DocToolOrderPlan
}

func (builder *docToolOrdersBuilder) finish(source DocToolOrderSource, state DocToolOrderState) error {
	plan, objects, err := FinishDocToolOrder(builder.buffer.ctx, builder.tree, builder.keys, source, state, builder.buffer.loadObject)
	if err != nil {
		return err
	}
	if err := builder.buffer.append(objects); err != nil {
		return err
	}
	if _, err := builder.buffer.put(plan.Encode(builder.tree, builder.keys)); err != nil {
		return err
	}
	builder.state.After, builder.state.Seen = source.Parent.Value.From, builder.state.Seen+1
	builder.state.Containers++
	builder.state.Current, builder.state.Active = nil, ""
	builder.plan = &plan
	return nil
}

func (builder *docToolOrdersBuilder) advance(tools DocToolPreparationReader) error {
	source := *builder.state.Current
	raw, err := builder.buffer.read(builder.state.Active, MaxDocDisplayStateBytes)
	if err != nil {
		return err
	}
	state, err := DecodeDocToolOrderState(builder.tree, builder.keys, source, builder.state.Active, raw)
	if err != nil {
		return err
	}
	if !state.Done(source) {
		step, err := StepDocToolOrder(builder.buffer.ctx, builder.tree, builder.keys, source, state,
			func(ctx context.Context, after *DocToolOrderEntry, limit int) ([]DocToolOrderEntry, error) {
				return tools.OrderEntries(ctx, source, after, limit)
			}, builder.buffer.loadObject)
		if err != nil {
			return err
		}
		if err := builder.buffer.append(step.Objects()); err != nil {
			return err
		}
		builder.order = &step
		state = step.State()
	}
	if state.Done(source) {
		return builder.finish(source, state)
	}
	builder.state.Active, err = builder.buffer.put(state.Encode(builder.tree, builder.keys, source))
	return err
}

func StepDocToolOrders(ctx context.Context, tree DocToolTreeSource, keys DocToolKeyMapSource, source DocToolOrdersSource, prior DocToolOrdersState, tools DocToolPreparationReader, loadObject func(context.Context, ContentHash) ([]byte, error)) (result DocToolOrdersStep, failure error) {
	defer func() {
		if err := ctx.Err(); err != nil {
			failure = err
		}
		if failure != nil {
			result = DocToolOrdersStep{}
		}
	}()
	if err := ctx.Err(); err != nil {
		return result, err
	}
	encoded, err := prior.Encode(tree, keys, source)
	if err != nil || prior.Done(source) || tools.Nodes == nil || tools.OrderSource == nil || tools.OrderEntries == nil || loadObject == nil {
		return result, ErrIntegrity
	}
	state, err := DecodeDocToolOrdersState(tree, keys, source, HashContent(encoded), encoded)
	if err != nil {
		return result, err
	}
	builder := docToolOrdersBuilder{tree: tree, keys: keys, source: source, state: state, buffer: docEventDisplayBuilder{ctx: ctx, load: loadObject}}
	limit := min(DocToolKeyMapStepNodes, source.Nodes-prior.Seen)
	if state.Current != nil {
		limit = 1
	}
	nodes, err := tools.Nodes(ctx, prior.After, limit)
	if err != nil {
		return result, err
	}
	if len(nodes) != limit {
		return result, ErrIntegrity
	}
	if state.Current != nil {
		actual, err := nodes[0].Encode(tree)
		expected, expectedErr := state.Current.Parent.Encode(tree)
		if err != nil || expectedErr != nil || !bytes.Equal(actual, expected) {
			return result, ErrIntegrity
		}
		builder.inputs = append(builder.inputs, cloneDocToolTreeNode(nodes[0]))
		if err := builder.advance(tools); err != nil {
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
			if node.Type != DocToolValueObject && node.Type != DocToolValueArray {
				builder.state.After, builder.state.Seen = node.Value.From, builder.state.Seen+1
				continue
			}
			container, err := tools.OrderSource(ctx, node.Value.From)
			if err != nil {
				return result, err
			}
			container.Parent = cloneDocToolTreeNode(container.Parent)
			parent, err := container.Parent.Encode(tree)
			original, originalErr := node.Encode(tree)
			if err != nil || originalErr != nil || !bytes.Equal(parent, original) || container.Validate(tree, keys) != nil {
				return result, ErrIntegrity
			}
			inner, err := NewDocToolOrderState(tree, keys, container)
			if err != nil {
				return result, err
			}
			if inner.Done(container) {
				if err := builder.finish(container, inner); err != nil {
					return result, err
				}
			} else if container.Count <= DocToolOrderPageEntries {
				step, err := StepDocToolOrder(ctx, tree, keys, container, inner,
					func(ctx context.Context, after *DocToolOrderEntry, limit int) ([]DocToolOrderEntry, error) {
						return tools.OrderEntries(ctx, container, after, limit)
					}, builder.buffer.loadObject)
				if err != nil {
					return result, err
				}
				if err := builder.buffer.append(step.Objects()); err != nil {
					return result, err
				}
				builder.order = &step
				if err := builder.finish(container, step.State()); err != nil {
					return result, err
				}
			} else {
				builder.state.Current = &container
				builder.state.Active, err = builder.buffer.put(inner.Encode(tree, keys, container))
				if err != nil {
					return result, err
				}
			}
			break
		}
	}
	next, err := builder.state.Encode(tree, keys, source)
	if err != nil || HashContent(next) == HashContent(encoded) {
		return result, ErrIntegrity
	}
	return DocToolOrdersStep{source: source, prior: HashContent(encoded), state: builder.state, inputs: builder.inputs,
		order: builder.order, plan: builder.plan, objects: builder.buffer.objects}, nil
}
