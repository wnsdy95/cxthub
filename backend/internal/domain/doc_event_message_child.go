package domain

import (
	"bytes"
	"context"
	"encoding/json"
)

const maxDocEventMessageChildBytes = 2048

type docEventMessageChildTask struct {
	Active  ContentHash `json:"active"`
	Block   int         `json:"block"`
	Inputs  ContentHash `json:"inputs"`
	Name    ContentHash `json:"name,omitempty"`
	Phase   string      `json:"phase"`
	Text    ContentHash `json:"text,omitempty"`
	Type    ContentHash `json:"type,omitempty"`
	Version int         `json:"version"`
}

func docEventMessageChildSources(input DocMessageInput) ([3]*DocDecodedTextSource, error) {
	var sources [3]*DocDecodedTextSource
	if input.source.Validate() != nil || input.entry.Validate(input.source) != nil || input.SummarySlot() {
		return sources, ErrIntegrity
	}
	var err error
	if input.IsText() {
		sources[0], err = input.TextSource()
		if err == nil && sources[0] == nil {
			err = ErrIntegrity
		}
	} else {
		sources[1], sources[2], err = input.PlaceholderSources()
	}
	return sources, err
}

func (task docEventMessageChildTask) validate(inputsHash ContentHash, input DocMessageInput) error {
	sources, err := docEventMessageChildSources(input)
	if err != nil || task.Version != 1 || ValidateContentHash(inputsHash) != nil || task.Inputs != inputsHash || task.Block != input.Index() || ValidateContentHash(task.Active) != nil {
		return ErrIntegrity
	}
	for _, hash := range []ContentHash{task.Text, task.Type, task.Name} {
		if hash != "" && ValidateContentHash(hash) != nil {
			return ErrIntegrity
		}
	}
	valid := false
	if input.IsText() {
		valid = task.Type == "" && task.Name == "" && (task.Phase == "text" && task.Text == "" || task.Phase == "markdown" && task.Text != "")
	} else if task.Text == "" {
		switch task.Phase {
		case "type":
			valid = sources[1] != nil && task.Type == "" && task.Name == ""
		case "name":
			valid = sources[2] != nil && (sources[1] == nil) == (task.Type == "") && task.Name == ""
		case "placeholder":
			valid = (sources[1] == nil) == (task.Type == "") && (sources[2] == nil) == (task.Name == "")
		}
	}
	if !valid {
		return ErrIntegrity
	}
	return nil
}

func (task docEventMessageChildTask) encode(inputsHash ContentHash, input DocMessageInput) ([]byte, error) {
	if task.validate(inputsHash, input) != nil {
		return nil, ErrIntegrity
	}
	raw, err := json.Marshal(task)
	if err != nil || len(raw) > maxDocEventMessageChildBytes {
		return nil, ErrIntegrity
	}
	return raw, nil
}

func decodeDocEventMessageChildTask(inputsHash ContentHash, input DocMessageInput, expected ContentHash, raw []byte) (docEventMessageChildTask, error) {
	var task docEventMessageChildTask
	if len(raw) == 0 || len(raw) > maxDocEventMessageChildBytes || ValidateContentHash(expected) != nil || HashContent(raw) != expected || json.Unmarshal(raw, &task) != nil {
		return docEventMessageChildTask{}, ErrIntegrity
	}
	canonical, err := task.encode(inputsHash, input)
	if err != nil || !bytes.Equal(canonical, raw) {
		return docEventMessageChildTask{}, ErrIntegrity
	}
	return task, nil
}

func (builder *docEventDisplayBuilder) startMessageChild(inputsHash ContentHash, input DocMessageInput) (childHash ContentHash, failure error) {
	priorObjects, priorBytes := len(builder.objects), builder.bytes
	defer func() {
		if err := builder.ctx.Err(); err != nil {
			failure = err
		}
		if failure != nil {
			childHash = ""
			clear(builder.objects[priorObjects:])
			builder.objects, builder.bytes = builder.objects[:priorObjects], priorBytes
		}
	}()
	if err := builder.ctx.Err(); err != nil {
		return "", err
	}
	sources, err := docEventMessageChildSources(input)
	if err != nil || ValidateContentHash(inputsHash) != nil {
		return "", ErrIntegrity
	}
	task := docEventMessageChildTask{Version: 1, Inputs: inputsHash, Block: input.Index()}
	for position, source := range sources {
		if source == nil {
			continue
		}
		state, err := NewDocDecodedTextState(*source)
		if err != nil {
			return "", err
		}
		task.Phase = []string{"text", "type", "name"}[position]
		task.Active, err = builder.put(state.Encode(*source))
		if err != nil {
			return "", err
		}
		return builder.put(task.encode(inputsHash, input))
	}
	if err := builder.initializeMessageChildPlaceholder(&task, input, nil, nil); err != nil {
		return "", err
	}
	return builder.put(task.encode(inputsHash, input))
}

func (builder *docEventDisplayBuilder) messageChildGeneration(hash ContentHash, source *DocDecodedTextSource) (*DocDecodedTextGeneration, error) {
	if source == nil || ValidateContentHash(hash) != nil {
		return nil, ErrIntegrity
	}
	raw, err := builder.read(hash, MaxDocDisplaySourceBytes)
	if err != nil {
		return nil, err
	}
	generation, err := DecodeDocDecodedTextGeneration(hash, raw)
	if err != nil || generation.Source != *source {
		return nil, ErrIntegrity
	}
	return &generation, nil
}

func (builder *docEventDisplayBuilder) initializeMessageChildPlaceholder(task *docEventMessageChildTask, input DocMessageInput, blockType, name *DocDecodedTextGeneration) error {
	plan, err := NewDocMessagePlaceholderPlan(input, blockType, name)
	if err != nil {
		return err
	}
	task.Phase = "placeholder"
	task.Active, err = builder.put(plan.Encode())
	return err
}

func (builder *docEventDisplayBuilder) advanceMessageChild(inputsHash ContentHash, input DocMessageInput, childHash ContentHash, loadRaw func(context.Context, int, int) ([]byte, error)) (nextHash ContentHash, plan DocMessageDisplayBlockPlan, done bool, failure error) {
	priorObjects, priorBytes := len(builder.objects), builder.bytes
	defer func() {
		if err := builder.ctx.Err(); err != nil {
			failure = err
		}
		if failure != nil {
			nextHash, plan, done = "", DocMessageDisplayBlockPlan{}, false
			clear(builder.objects[priorObjects:])
			builder.objects, builder.bytes = builder.objects[:priorObjects], priorBytes
		}
	}()
	if err := builder.ctx.Err(); err != nil {
		return "", plan, false, err
	}
	raw, err := builder.read(childHash, maxDocEventMessageChildBytes)
	if err != nil {
		return "", plan, false, err
	}
	task, err := decodeDocEventMessageChildTask(inputsHash, input, childHash, raw)
	if err != nil {
		return "", plan, false, err
	}
	sources, err := docEventMessageChildSources(input)
	if err != nil {
		return "", plan, false, err
	}
	var generations [3]*DocDecodedTextGeneration
	for position, hash := range []ContentHash{task.Text, task.Type, task.Name} {
		if hash != "" {
			generations[position], err = builder.messageChildGeneration(hash, sources[position])
			if err != nil {
				return "", plan, false, err
			}
		}
	}
	raw, err = builder.read(task.Active, MaxDocDisplayStateBytes)
	if err != nil {
		return "", plan, false, err
	}
	switch task.Phase {
	case "placeholder":
		placeholder, err := DecodeDocMessagePlaceholderPlan(task.Active, raw)
		if err != nil {
			return "", plan, false, err
		}
		expected, err := NewDocMessagePlaceholderPlan(input, generations[1], generations[2])
		if err != nil {
			return "", plan, false, err
		}
		canonical, err := expected.Encode()
		if err != nil || !bytes.Equal(raw, canonical) {
			return "", plan, false, ErrIntegrity
		}
		return "", DocMessageDisplayBlockPlan{Placeholder: &placeholder}, true, nil
	case "markdown":
		state, err := DecodeDocMarkdownPlanState(task.Active, raw)
		if err != nil || state.Done() || state.Input != (DocMarkdownPlanInput{Hash: generations[0].Hash, Bytes: generations[0].TextBytes}) {
			return "", plan, false, ErrIntegrity
		}
		step, err := StepDocDecodedMarkdownPlan(builder.ctx, *generations[0], state, builder.loadObject)
		if err != nil {
			return "", plan, false, err
		}
		if err := builder.append(step.Objects); err != nil {
			return "", plan, false, err
		}
		if !step.State.Done() {
			task.Active, err = builder.put(step.State.Encode())
			if err != nil {
				return "", plan, false, err
			}
			break
		}
		markdown, objects, err := FinishDocMarkdownPlan(builder.ctx, step.State)
		if err != nil {
			return "", plan, false, err
		}
		if err := builder.append(objects); err != nil {
			return "", plan, false, err
		}
		decoded := DocDecodedMarkdownPlan{Version: 1, Input: *generations[0], Markdown: markdown}
		if _, err := builder.put(decoded.Encode()); err != nil {
			return "", plan, false, err
		}
		return "", DocMessageDisplayBlockPlan{Markdown: &decoded}, true, nil
	case "text", "type", "name":
		position := 0
		if task.Phase == "type" {
			position = 1
		} else if task.Phase == "name" {
			position = 2
		}
		source := *sources[position]
		state, err := DecodeDocDecodedTextState(source, task.Active, raw)
		if err != nil || state.Done {
			return "", plan, false, ErrIntegrity
		}
		step, err := StepDocDecodedText(builder.ctx, source, state, loadRaw, builder.loadObject)
		if err != nil {
			return "", plan, false, err
		}
		if err := builder.append(step.Objects); err != nil {
			return "", plan, false, err
		}
		if !step.State.Done {
			task.Active, err = builder.put(step.State.Encode(source))
			if err != nil {
				return "", plan, false, err
			}
			break
		}
		generation, objects, err := FinishDocDecodedText(builder.ctx, source, step.State)
		if err != nil {
			return "", plan, false, err
		}
		if err := builder.append(objects); err != nil {
			return "", plan, false, err
		}
		hash, err := builder.put(generation.Encode())
		if err != nil {
			return "", plan, false, err
		}
		generations[position] = &generation
		switch task.Phase {
		case "text":
			task.Text, task.Phase = hash, "markdown"
			markdown, failure := NewDocMarkdownPlanState(DocMarkdownPlanInput{Hash: generation.Hash, Bytes: generation.TextBytes})
			if failure != nil {
				return "", plan, false, failure
			}
			task.Active, err = builder.put(markdown.Encode())
		case "type":
			task.Type = hash
			if sources[2] != nil {
				task.Phase = "name"
				name, failure := NewDocDecodedTextState(*sources[2])
				if failure != nil {
					return "", plan, false, failure
				}
				task.Active, err = builder.put(name.Encode(*sources[2]))
			} else {
				err = builder.initializeMessageChildPlaceholder(&task, input, generations[1], nil)
			}
		case "name":
			task.Name = hash
			err = builder.initializeMessageChildPlaceholder(&task, input, generations[1], generations[2])
		}
		if err != nil {
			return "", plan, false, err
		}
	default:
		return "", plan, false, ErrIntegrity
	}
	nextHash, err = builder.put(task.encode(inputsHash, input))
	if err != nil || nextHash == childHash {
		if err == nil {
			err = ErrIntegrity
		}
		return "", plan, false, err
	}
	return nextHash, plan, false, nil
}
