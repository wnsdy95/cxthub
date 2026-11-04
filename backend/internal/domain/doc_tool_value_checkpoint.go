package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
)

func (builder *docToolValueBuilder) framePlan(frame docToolValueFrame) (DocToolOrderPlan, error) {
	if frame.Plan == builder.planHash {
		if !sameDocToolValueNode(builder.source.Tree, builder.plan.Source.Parent, frame.Node) || frame.Next > builder.plan.Source.Count || frame.Child != nil && frame.Next == builder.plan.Source.Count {
			return DocToolOrderPlan{}, ErrIntegrity
		}
		return builder.plan, nil
	}
	raw, err := builder.buffer.read(frame.Plan, MaxDocDisplaySourceBytes)
	if err != nil {
		return DocToolOrderPlan{}, err
	}
	plan, err := DecodeDocToolOrderPlan(builder.source.Tree, builder.source.Keys, frame.Plan, raw)
	if err != nil || !sameDocToolValueNode(builder.source.Tree, plan.Source.Parent, frame.Node) || frame.Next > plan.Source.Count || frame.Child != nil && frame.Next == plan.Source.Count {
		return DocToolOrderPlan{}, ErrIntegrity
	}
	builder.planHash, builder.plan = frame.Plan, plan
	return plan, nil
}

func (builder *docToolValueBuilder) entry(frame docToolValueFrame, index int) (DocToolOrderEntry, error) {
	plan, err := builder.framePlan(frame)
	if err != nil {
		return DocToolOrderEntry{}, err
	}
	if index < 0 || index >= plan.Source.Count {
		return DocToolOrderEntry{}, ErrIntegrity
	}
	hash, err := plan.Source.Hash(builder.source.Tree, builder.source.Keys)
	if err != nil {
		return DocToolOrderEntry{}, err
	}
	pageIndex := index / DocToolOrderPageEntries
	if builder.orderKey != hash || builder.order.Index != pageIndex {
		page, err := ReadDocToolOrderPage(builder.buffer.ctx, builder.source.Tree, builder.source.Keys, plan, pageIndex, builder.buffer.loadObject)
		if err != nil {
			return DocToolOrderEntry{}, err
		}
		builder.orderKey, builder.order = hash, page
	}
	return builder.order.Entries[index%DocToolOrderPageEntries], nil
}

func (builder *docToolValueBuilder) validateFrame(frame docToolValueFrame) error {
	if frame.validate(builder.source) != nil {
		return ErrIntegrity
	}
	if _, err := builder.framePlan(frame); err != nil {
		return err
	}
	if frame.Next > 0 {
		previous, err := builder.entry(frame, frame.Next-1)
		if err != nil {
			return err
		}
		if frame.Previous != previous.Node.Value.To {
			return ErrIntegrity
		}
	}
	if frame.Child != nil {
		selected, err := builder.entry(frame, frame.Next)
		if err != nil {
			return err
		}
		if !sameDocToolValueNode(builder.source.Tree, selected.Node, *frame.Child) {
			return ErrIntegrity
		}
	}
	return nil
}

func (builder *docToolValueBuilder) checkpoint() error {
	if err := builder.buffer.ctx.Err(); err != nil {
		return err
	}
	if builder.state.Validate(builder.source) != nil {
		return ErrIntegrity
	}
	if builder.state.Receipt != "" {
		raw, err := builder.buffer.read(builder.state.Receipt, MaxDocDisplaySourceBytes)
		if err != nil {
			return err
		}
		var receipt docToolValueReceipt
		if json.Unmarshal(raw, &receipt) != nil {
			return ErrIntegrity
		}
		encoded, err := canonicalJSON(receipt)
		digest, digestErr := builder.state.controlDigest()
		if err != nil || digestErr != nil || !bytes.Equal(raw, encoded) || receipt.Version != 1 || receipt.Profile != "public-tool-value-receipt-v1" || receipt.Source != builder.state.Source || receipt.Digest != digest {
			return ErrIntegrity
		}
	}
	var tail DocToolValuePage
	end := builder.state.TextBytes
	if builder.state.Pending != "" {
		raw, err := builder.buffer.read(builder.state.Pending, MaxDocDisplayPageBytes)
		if err != nil {
			return err
		}
		page, err := DecodeDocToolValuePage(builder.source, builder.state.Pending, raw)
		if err != nil || page.Sealed || page.Display.Index != builder.state.Directory.Count || page.Display.From+len(page.Display.Text) != end {
			return ErrIntegrity
		}
		builder.page, tail, end = page, page, page.Display.From
		builder.page.Display.Runs = append([]DocDisplayRun(nil), page.Display.Runs...)
		builder.text.WriteString(page.Display.Text)
		builder.page.Display.Text = ""
		for _, character := range page.Display.Text {
			builder.encoded += docDisplayEncodedRuneBytes(character)
		}
	} else {
		builder.resetPage()
	}
	if builder.state.Directory.Count > 0 {
		root, objects, err := builder.state.Directory.Finish(builder.buffer.ctx)
		if err != nil {
			return err
		}
		load := func(ctx context.Context, hash ContentHash) ([]byte, error) {
			for _, object := range objects {
				if object.Hash == hash {
					return object.Bytes, nil
				}
			}
			return builder.buffer.loadObject(ctx, hash)
		}
		hash, err := ResolveDocRangeLeaf(builder.buffer.ctx, root, root.Count-1, load)
		if err != nil {
			return err
		}
		raw, err := load(builder.buffer.ctx, hash)
		if err != nil {
			return err
		}
		page, err := DecodeDocToolValuePage(builder.source, hash, raw)
		if err != nil || !page.Sealed || page.Final != builder.state.Done() || page.Display.Index != root.Count-1 || page.Display.From+len(page.Display.Text) != end {
			return ErrIntegrity
		}
		if tail.Display.Text == "" {
			tail = page
		}
	} else if end != 0 {
		return ErrIntegrity
	}
	if builder.state.Frame != nil {
		if err := builder.validateFrame(*builder.state.Frame); err != nil {
			return err
		}
		if frame := builder.state.Frame; frame.Parent != "" {
			builder.frames++
			raw, err := builder.buffer.read(frame.Parent, MaxDocDisplaySourceBytes)
			if err != nil {
				return err
			}
			parent, err := decodeDocToolValueFrame(builder.source, frame.Parent, raw)
			if err != nil || parent.Child == nil || !sameDocToolValueNode(builder.source.Tree, *parent.Child, frame.Node) {
				return ErrIntegrity
			}
			if err := builder.validateFrame(parent); err != nil {
				return err
			}
		}
	}
	if len(tail.Display.Runs) > 0 {
		last := tail.Display.Runs[len(tail.Display.Runs)-1]
		switch builder.state.Phase {
		case "next":
			end := builder.state.Frame.Previous
			if builder.state.Frame.Next == 0 {
				end = builder.state.Frame.Node.Value.From + 1
			}
			if last.SourceTo != end {
				return ErrIntegrity
			}
		case "colon":
			if !strings.HasSuffix(tail.Display.Text, "\"") || !last.Linear || last.SourceTo != builder.state.Frame.Child.Key.To {
				return ErrIntegrity
			}
		case "value", "number":
			if frame := builder.state.Frame; frame != nil && frame.Node.Type == DocToolValueObject {
				suffix := ": "[2-min(2, len(tail.Display.Text)):]
				if !strings.HasSuffix(tail.Display.Text, suffix) || last.Linear || last.SourceTo-last.SourceFrom != 1 || last.SourceFrom < frame.Child.Key.To || last.SourceTo > frame.Child.Value.From {
					return ErrIntegrity
				}
			}
		case "complete":
			if last.SourceTo != builder.source.Root.Value.To {
				return ErrIntegrity
			}
		}
	}
	if builder.state.Active != "" {
		raw, err := builder.buffer.read(builder.state.Active, MaxDocDisplayStateBytes)
		if err != nil {
			return err
		}
		if builder.state.Phase == "number" {
			source, err := NewDocToolNumberSource(builder.source.Tree, *builder.state.Value)
			if err != nil {
				return err
			}
			state, err := DecodeDocToolNumberState(source, builder.state.Active, raw)
			if err != nil || state.Done() || builder.state.Start != builder.state.TextBytes {
				return ErrIntegrity
			}
		} else {
			source := builder.stringSource()
			state, err := DecodeDocToolStringState(source, builder.state.Active, raw)
			if err != nil || state.Done || builder.state.Start+state.TextBytes != builder.state.TextBytes || len(tail.Display.Runs) == 0 || tail.Display.Runs[len(tail.Display.Runs)-1].SourceTo != state.Next {
				return ErrIntegrity
			}
			if err := ValidateDocToolStringCheckpoint(builder.buffer.ctx, source, state, builder.buffer.loadObject); err != nil {
				return err
			}
		}
	}
	return nil
}
