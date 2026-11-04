package domain

import "context"

func (builder *docEventDisplayBuilder) toolPolicyDecode(state *docToolPolicyState, token DocFindSpan, loadRaw func(context.Context, int, int) ([]byte, error)) (bool, error) {
	source := DocDecodedTextSource{Version: 1, Profile: DocDecodedTextProfile, EventHash: state.Source.Event.EventHash, EventBytes: state.Source.Event.EventBytes, Token: token}
	if state.Active == "" {
		decoded, err := NewDocDecodedTextState(source)
		if err != nil {
			return false, err
		}
		state.Stats = &docToolPolicyText{Token: token}
		state.Active, err = builder.put(decoded.Encode(source))
		return false, err
	}
	if state.Stats == nil || state.Stats.Token != token {
		return false, ErrIntegrity
	}
	raw, err := builder.read(state.Active, MaxDocDisplayStateBytes)
	if err != nil {
		return false, err
	}
	decoded, err := DecodeDocDecodedTextState(source, state.Active, raw)
	if err != nil || decoded.TextBytes != state.Stats.Bytes {
		return false, ErrIntegrity
	}
	if !decoded.Done {
		step, err := StepDocDecodedText(builder.ctx, source, decoded, loadRaw, builder.loadObject)
		if err != nil {
			return false, err
		}
		if err := builder.append(step.Objects); err != nil {
			return false, err
		}
		if step.Page != nil {
			if err := state.Stats.advance(*step.Page); err != nil {
				return false, err
			}
		}
		decoded = step.State
	}
	if !decoded.Done {
		state.Active, err = builder.put(decoded.Encode(source))
		return false, err
	}
	generation, objects, err := FinishDocDecodedText(builder.ctx, source, decoded)
	if err != nil {
		return false, err
	}
	if err := builder.append(objects); err != nil {
		return false, err
	}
	state.Stats.Generation, err = builder.put(generation.Encode())
	state.Active = ""
	return true, err
}

func (builder *docEventDisplayBuilder) toolPolicyHeader(state *docToolPolicyState, fields DocToolFields, loadRaw func(context.Context, int, int) ([]byte, error)) error {
	if fields.Kind == EventToolResult {
		state.Phase = "layout"
		return nil
	}
	var token *DocFindSpan
	if state.Phase == "name" {
		for _, candidate := range []*DocFindSpan{fields.ProviderToolName, fields.ToolName} {
			if candidate != nil && candidate.To-candidate.From > 2 {
				token = candidate
				break
			}
		}
	} else {
		for _, candidate := range state.Members[:4] {
			if candidate != nil && candidate.Type == DocToolValueString && candidate.Value.To-candidate.Value.From > 2 {
				span := candidate.Value
				token = &span
				break
			}
		}
	}
	complete := token == nil
	if token != nil {
		var err error
		complete, err = builder.toolPolicyDecode(state, *token, loadRaw)
		if err != nil || !complete {
			return err
		}
	}
	if complete {
		if state.Phase == "name" {
			state.Name, state.Phase = state.Stats, "head"
		} else {
			state.Head, state.Phase = state.Stats, "truth"
		}
		state.Stats = nil
	}
	return nil
}

func (state docToolPolicyState) name() string {
	if state.Name == nil {
		return "tool"
	}
	if state.Name.Bytes != len(state.Name.Prefix) {
		return ""
	}
	return string(state.Name.Prefix)
}

func (builder *docEventDisplayBuilder) toolPolicyTruth(state *docToolPolicyState, loadRaw func(context.Context, int, int) ([]byte, error)) error {
	if state.TruthAt == len(state.Truth) {
		state.Phase = "layout"
		return nil
	}
	name := state.name()
	relevant := name == "Edit" && state.TruthAt < 2 || name == "Write" && state.TruthAt == 2 || name == "Bash" && state.TruthAt == 3
	member := state.Members[[4]int{4, 5, 6, 1}[state.TruthAt]]
	if !relevant || member == nil {
		state.TruthAt++
		return nil
	}
	truth := false
	switch member.Type {
	case DocToolValueObject, DocToolValueArray, DocToolValueTrue:
		truth = true
	case DocToolValueString:
		truth = member.Value.To-member.Value.From > 2
	case DocToolValueNumber:
		source, err := NewDocToolNumberSource(*state.Source.Tree, *member)
		if err != nil {
			return err
		}
		number, err := NewDocToolNumberState(source)
		if err != nil {
			return err
		}
		if state.Active != "" {
			raw, err := builder.read(state.Active, MaxDocToolNumberStateBytes)
			if err != nil {
				return err
			}
			number, err = DecodeDocToolNumberState(source, state.Active, raw)
			if err != nil {
				return err
			}
		}
		if !number.Done() {
			step, err := StepDocToolNumber(builder.ctx, source, number, loadRaw)
			if err != nil {
				return err
			}
			number = step.State()
		}
		if !number.Done() {
			state.Active, err = builder.put(number.Encode(source))
			return err
		}
		plan, err := FinishDocToolNumber(builder.ctx, source, number)
		if err != nil {
			return err
		}
		truth = plan.Text != "0"
		state.Active = ""
	}
	state.Truth[state.TruthAt] = truth
	state.TruthAt++
	return nil
}

func (state *docToolPolicyState) literal(text string, token DocFindSpan) {
	state.Parts = append(state.Parts, docToolEventPart{Kind: "literal", Token: token, Text: text, TextBytes: len(text)})
}

func (state *docToolPolicyState) layout(fields DocToolFields) error {
	if state.Parts != nil || state.Part != 0 || state.Active != "" || state.Stats != nil {
		return ErrIntegrity
	}
	if fields.Kind == EventToolResult {
		if fields.Output != nil && fields.OutputType == DocToolValueString {
			state.Parts = []docToolEventPart{{Kind: "decoded", Token: *fields.Output}}
		}
		state.Phase = "parts"
		return nil
	}
	if state.Name == nil {
		state.literal("tool", fields.KindToken)
	} else {
		state.Parts = append(state.Parts, docToolEventPart{Kind: "decoded", Token: state.Name.Token, Plan: state.Name.Generation, TextBytes: state.Name.Bytes})
	}
	if state.Head != nil {
		anchor := DocFindSpan{From: state.Head.Token.From, To: state.Head.Token.From + 1}
		state.literal("(", anchor)
		skip := 0
		if state.Members[0] != nil && state.Members[0].Value == state.Head.Token && state.Head.Slashes > 3 {
			skip = state.Head.Last[0] + 1
			state.literal("…/", anchor)
		}
		state.Parts = append(state.Parts, docToolEventPart{Kind: "decoded", Token: state.Head.Token, Plan: state.Head.Generation, Skip: skip, TextBytes: state.Head.Bytes - skip})
		state.literal(")", DocFindSpan{From: state.Head.Token.To - 1, To: state.Head.Token.To})
	}
	name := state.name()
	switch {
	case name == "Edit" && (state.Truth[0] || state.Truth[1]):
		for index, member := range state.Members[4:6] {
			if member != nil && member.Type == DocToolValueString && member.Value.To-member.Value.From > 2 {
				state.literal("\n", DocFindSpan{From: member.Value.From, To: member.Value.From + 1})
				mode := DocToolStringRemove
				if index == 1 {
					mode = DocToolStringAdd
				}
				state.Parts = append(state.Parts, docToolEventPart{Kind: "diff", Token: member.Value, Mode: mode})
			}
		}
	case name == "Write" && state.Truth[2]:
		member := state.Members[6]
		anchor := DocFindSpan{From: member.Value.From, To: member.Value.From + 1}
		state.literal("\n", anchor)
		if member.Type == DocToolValueString {
			state.Parts = append(state.Parts, docToolEventPart{Kind: "diff", Token: member.Value, Mode: DocToolStringAdd})
		} else {
			state.literal("+ ", anchor)
		}
	case name == "Bash" && state.Truth[3]:
		member := state.Members[1]
		state.literal("\n$ ", DocFindSpan{From: member.Value.From, To: member.Value.From + 1})
		if member.Type == DocToolValueString {
			state.Parts = append(state.Parts, docToolEventPart{Kind: "decoded", Token: member.Value})
		}
	default:
		if state.Source.RootCount > 0 {
			state.literal("\n", DocFindSpan{From: state.Source.Tree.Root.From, To: state.Source.Tree.Root.From + 1})
			state.Parts = append(state.Parts, docToolEventPart{Kind: "value", Token: state.Source.Tree.Root})
		}
	}
	state.Phase = "parts"
	return nil
}
