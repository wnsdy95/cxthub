package domain

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

type eventMessageChildTestFixture struct {
	raw        []byte
	inputsHash ContentHash
	inputs     []DocMessageInput
	objects    map[ContentHash][]byte
}

func newEventMessageChildTestFixture(test *testing.T, raw []byte) *eventMessageChildTestFixture {
	test.Helper()
	prepared := buildMessageInputFixture(test, raw)
	encoded, err := prepared.generation.Encode()
	if err != nil {
		test.Fatal(err)
	}
	return &eventMessageChildTestFixture{raw: raw, inputsHash: HashContent(encoded), inputs: messageFixtureInputs(test, prepared), objects: prepared.objects}
}

func (fixture *eventMessageChildTestFixture) load(ctx context.Context, hash ContentHash) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	raw, found := fixture.objects[hash]
	if !found {
		return nil, ErrNotFound
	}
	return bytes.Clone(raw), nil
}

func (fixture *eventMessageChildTestFixture) store(raw []byte) ContentHash {
	hash := HashContent(raw)
	fixture.objects[hash] = bytes.Clone(raw)
	return hash
}

func (fixture *eventMessageChildTestFixture) persist(test *testing.T, builder docEventDisplayBuilder) {
	test.Helper()
	total := 0
	for _, object := range builder.objects {
		if HashContent(object.Bytes) != object.Hash || len(object.Bytes) > MaxDocDisplayPageBytes {
			test.Fatal("invalid staged object")
		}
		if previous, found := fixture.objects[object.Hash]; found && !bytes.Equal(previous, object.Bytes) {
			test.Fatal("immutable object changed")
		}
		total += len(object.Bytes)
		fixture.store(object.Bytes)
	}
	if total != builder.bytes || total > MaxDocEventDisplayStepBytes || len(builder.objects) > MaxDocEventDisplayStepObjects {
		test.Fatal("step exceeded its staging budget", total, len(builder.objects))
	}
}

func (fixture *eventMessageChildTestFixture) task(test *testing.T, input DocMessageInput, hash ContentHash) docEventMessageChildTask {
	test.Helper()
	raw := fixture.objects[hash]
	task, err := decodeDocEventMessageChildTask(fixture.inputsHash, input, hash, raw)
	if err != nil || len(raw) > 2048 || task.Inputs != fixture.inputsHash || task.Block != input.Index() {
		test.Fatal("invalid child checkpoint", err)
	}
	for _, checkpoint := range [][]byte{raw, fixture.objects[task.Active]} {
		if len(checkpoint) == 0 || len(checkpoint) > 64<<10 || bytes.Contains(checkpoint, []byte("PUBLIC_BODY_MARKER")) || bytes.Contains(checkpoint, []byte("PRIVATE_BODY_MARKER")) {
			test.Fatal("unbounded or body-retaining checkpoint")
		}
	}
	return task
}

func (fixture *eventMessageChildTestFixture) start(test *testing.T, input DocMessageInput) ContentHash {
	test.Helper()
	builder := docEventDisplayBuilder{ctx: context.Background(), load: fixture.load}
	hash, err := builder.startMessageChild(fixture.inputsHash, input)
	if err != nil {
		test.Fatal("start child", err)
	}
	fixture.persist(test, builder)
	fixture.task(test, input, hash)
	return hash
}

func (fixture *eventMessageChildTestFixture) advance(test *testing.T, input DocMessageInput, hash ContentHash) (ContentHash, DocMessageDisplayBlockPlan, bool) {
	test.Helper()
	task := fixture.task(test, input, hash)
	sources, err := docEventMessageChildSources(input)
	if err != nil {
		test.Fatal(err)
	}
	var selected *DocDecodedTextSource
	switch task.Phase {
	case "text":
		selected = sources[0]
	case "type":
		selected = sources[1]
	case "name":
		selected = sources[2]
	}
	readBytes := 0
	builder := docEventDisplayBuilder{ctx: context.Background(), load: fixture.load}
	next, plan, done, err := builder.advanceMessageChild(fixture.inputsHash, input, hash, func(ctx context.Context, from, length int) ([]byte, error) {
		if ctx.Err() != nil || selected == nil || length <= 0 || length > 4096 || from < selected.Token.From || from > selected.Token.To-length {
			test.Fatal("advance read outside its single selected token", task.Phase, from, length)
		}
		readBytes += length
		return bytes.Clone(fixture.raw[from : from+length]), nil
	})
	if err != nil || readBytes > DocDisplayStepSourceBytes {
		test.Fatal("advance child", task.Phase, readBytes, err)
	}
	fixture.persist(test, builder)
	if done {
		if next != "" || (plan.Markdown == nil) == (plan.Placeholder == nil) {
			test.Fatal("finished child did not return exactly one typed plan")
		}
	} else {
		if next == "" || next == hash || !reflect.DeepEqual(plan, DocMessageDisplayBlockPlan{}) {
			test.Fatal("unfinished child exposed a plan or stalled")
		}
		fixture.task(test, input, next)
	}
	return next, plan, done
}

func (fixture *eventMessageChildTestFixture) checkpoint(test *testing.T, input DocMessageInput, phase string) (ContentHash, docEventMessageChildTask) {
	test.Helper()
	hash := fixture.start(test, input)
	for steps := 0; steps < 4096; steps++ {
		task := fixture.task(test, input, hash)
		if task.Phase == phase {
			return hash, task
		}
		next, _, done := fixture.advance(test, input, hash)
		if done {
			test.Fatal("child completed before requested phase", phase)
		}
		hash = next
	}
	test.Fatal("child did not reach phase", phase)
	return "", docEventMessageChildTask{}
}

func eventMessageChildTestJSON(test *testing.T, value any) []byte {
	test.Helper()
	raw, err := canonicalJSON(value)
	if err != nil {
		test.Fatal(err)
	}
	return raw
}

func TestDocEventMessageChildRestartAndTypedParity(test *testing.T) {
	text := "# PUBLIC_BODY_MARKER\n\n" + strings.Repeat("**\ud55c\uae00🙂** [label](https://hidden.test)\n\n", 1800)
	name := "PUBLIC_BODY_MARKER **literal**\n" + strings.Repeat("`\ud55c\uae00🙂` [x](hidden)  ", 3000) + " tail  "
	raw := []byte(`{"kind":"message","opaque":{"secret":"` + strings.Repeat("PRIVATE_BODY_MARKER", 5000) + `"},"blocks":[{},{"type":"text","text":null},{"type":"text","text":` + string(eventMessageChildTestJSON(test, text)) + `},{"type":"tool","name":` + string(eventMessageChildTestJSON(test, name)) + `}]}`)
	fixture := newEventMessageChildTestFixture(test, raw)
	expected := messageDisplayChildren(test, raw, fixture.inputs, make(map[ContentHash][]byte))
	for _, index := range []int{0, 2, 3} {
		input := fixture.inputs[index]
		hash := fixture.start(test, input)
		phases := make(map[string]int)
		finished := false
		for steps := 0; steps < 4096; steps++ {
			task := fixture.task(test, input, hash)
			phases[task.Phase]++
			next, plan, done := fixture.advance(test, input, hash)
			if done {
				if !reflect.DeepEqual(plan, expected[index]) {
					test.Fatal("typed child differed from existing primitives", index)
				}
				if index == 3 {
					cursor, err := NewDocMessagePlaceholderCursor(*plan.Placeholder)
					if err != nil {
						test.Fatal(err)
					}
					var output strings.Builder
					for cursor.From < plan.Placeholder.TextBytes {
						fragment, err := ReadDocMessagePlaceholderFragment(context.Background(), *plan.Placeholder, cursor, fixture.load)
						if err != nil {
							test.Fatal(err)
						}
						output.WriteString(fragment.Text())
						cursor = fragment.Next()
					}
					if output.String() != "[tool: "+name+"]" {
						test.Fatal("large placeholder name was truncated or treated as Markdown")
					}
				}
				finished = true
				break
			}
			hash = next
		}
		if !finished || index == 2 && (phases["text"] < 2 || phases["markdown"] < 2) || index == 3 && (phases["type"] != 1 || phases["name"] < 2 || phases["markdown"] != 0) {
			test.Fatal("child did not make bounded sequential progress", index, phases)
		}
	}
}

func TestDocEventMessageChildPlaceholderLiteralAndAbsentTokens(test *testing.T) {
	for _, sample := range []struct {
		block string
		want  string
	}{
		{`{}`, "[]"},
		{`{"type":"image"}`, "[image]"},
		{`{"name":"**literal**\\tail\n  "}`, "[: **literal**\\tail\n  ]"},
		{`{"type":"","name":""}`, "[]"},
		{`{"type":"tool","name":"[link](private) ` + "`code`" + `"}`, "[tool: [link](private) `code`]"},
		{`{"type":null,"name":null}`, "[]"},
		{`{"type":"text","text":""}`, ""},
	} {
		test.Run(sample.block, func(test *testing.T) {
			fixture := newEventMessageChildTestFixture(test, []byte(`{"kind":"message","blocks":[`+sample.block+`]}`))
			input := fixture.inputs[0]
			hash := fixture.start(test, input)
			for steps := 0; steps < 100; steps++ {
				next, plan, done := fixture.advance(test, input, hash)
				if !done {
					hash = next
					continue
				}
				if input.IsText() {
					if plan.Markdown == nil || plan.Markdown.Markdown.TextBytes != 0 {
						test.Fatal("empty text lost its typed Markdown plan")
					}
					return
				}
				cursor, err := NewDocMessagePlaceholderCursor(*plan.Placeholder)
				if err != nil {
					test.Fatal(err)
				}
				var output strings.Builder
				for cursor.From < plan.Placeholder.TextBytes {
					fragment, err := ReadDocMessagePlaceholderFragment(context.Background(), *plan.Placeholder, cursor, fixture.load)
					if err != nil {
						test.Fatal(err)
					}
					output.WriteString(fragment.Text())
					cursor = fragment.Next()
				}
				if output.String() != sample.want || (input.entry.Type == nil) != (plan.Placeholder.Type == nil) || (input.entry.Name == nil) != (plan.Placeholder.Name == nil) {
					test.Fatal("placeholder changed literals or fabricated absent tokens", output.String())
				}
				return
			}
			test.Fatal("child stalled")
		})
	}
}

func TestDocEventMessageChildRejectsAdmission(test *testing.T) {
	fixture := newEventMessageChildTestFixture(test, []byte(`{"kind":"message","blocks":[{"type":"text"},{"type":"text","text":"ok"}]}`))
	for _, sample := range []struct {
		name  string
		input DocMessageInput
		hash  ContentHash
	}{
		{"nil-text", fixture.inputs[0], fixture.inputsHash},
		{"zero-input", DocMessageInput{}, fixture.inputsHash},
		{"bad-inputs-hash", fixture.inputs[1], "invalid"},
		{"summary", DocMessageInput{source: DocMessageInputSource{Version: 1, Profile: DocMessageInputProfile, EventHash: fixture.inputs[1].source.EventHash, EventBytes: len(fixture.raw), Blocks: fixture.inputs[1].source.Blocks, Summary: true}, entry: fixture.inputs[1].entry}, fixture.inputsHash},
	} {
		test.Run(sample.name, func(test *testing.T) {
			builder := docEventDisplayBuilder{ctx: context.Background(), load: fixture.load}
			hash, err := builder.startMessageChild(sample.hash, sample.input)
			if !errors.Is(err, ErrIntegrity) || hash != "" || len(builder.objects) != 0 || builder.bytes != 0 {
				test.Fatal("invalid admission exposed a child", hash, err)
			}
		})
	}
}

func eventMessageChildTestFailure(test *testing.T, fixture *eventMessageChildTestFixture, builder *docEventDisplayBuilder, inputsHash ContentHash, input DocMessageInput, childHash ContentHash, loadRaw func(context.Context, int, int) ([]byte, error), expected error) {
	test.Helper()
	priorObjects := append([]DocRangeObject(nil), builder.objects...)
	priorBytes := builder.bytes
	priorChild := bytes.Clone(fixture.objects[childHash])
	next, plan, done, err := builder.advanceMessageChild(inputsHash, input, childHash, loadRaw)
	if !errors.Is(err, expected) || next != "" || done || !reflect.DeepEqual(plan, DocMessageDisplayBlockPlan{}) || len(builder.objects) != len(priorObjects) || builder.bytes != priorBytes || !bytes.Equal(priorChild, fixture.objects[childHash]) {
		test.Fatal("failure exposed partial child output", next, done, err)
	}
	for index := range priorObjects {
		if !reflect.DeepEqual(priorObjects[index], builder.objects[index]) {
			test.Fatal("failure changed previously staged objects")
		}
	}
}

func TestDocEventMessageChildRejectsTaskShapeAndCanonicalEncoding(test *testing.T) {
	fixture := newEventMessageChildTestFixture(test, []byte(`{"kind":"message","blocks":[{"type":"text","text":"same"},{"type":"text","text":"same"}]}`))
	input := fixture.inputs[0]
	hash := fixture.start(test, input)
	task := fixture.task(test, input, hash)
	for _, sample := range []struct {
		name   string
		mutate func(*docEventMessageChildTask)
	}{
		{"version", func(task *docEventMessageChildTask) { task.Version++ }},
		{"inputs", func(task *docEventMessageChildTask) { task.Inputs = HashContent([]byte("other generation")) }},
		{"block", func(task *docEventMessageChildTask) { task.Block++ }},
		{"phase", func(task *docEventMessageChildTask) { task.Phase = "completed" }},
		{"markdown-without-text", func(task *docEventMessageChildTask) { task.Phase = "markdown" }},
		{"type-in-text", func(task *docEventMessageChildTask) { task.Type = hash }},
		{"name-in-text", func(task *docEventMessageChildTask) { task.Name = hash }},
		{"decoded-in-text", func(task *docEventMessageChildTask) { task.Text = hash }},
		{"missing-active", func(task *docEventMessageChildTask) { task.Active = "" }},
		{"bad-active", func(task *docEventMessageChildTask) { task.Active = "bad" }},
	} {
		test.Run(sample.name, func(test *testing.T) {
			changed := task
			sample.mutate(&changed)
			changedHash := fixture.store(eventMessageChildTestJSON(test, changed))
			builder := docEventDisplayBuilder{ctx: context.Background(), load: fixture.load}
			eventMessageChildTestFailure(test, fixture, &builder, fixture.inputsHash, input, changedHash, nil, ErrIntegrity)
		})
	}
	encoded := fixture.objects[hash]
	for _, raw := range [][]byte{
		nil,
		append(bytes.Clone(encoded), ' '),
		append([]byte(`{"unknown":1,`), encoded[1:]...),
		append([]byte(`{"version":1,`), encoded[1:]...),
		bytes.Repeat([]byte(" "), maxDocEventMessageChildBytes+1),
	} {
		changedHash := fixture.store(raw)
		builder := docEventDisplayBuilder{ctx: context.Background(), load: fixture.load}
		eventMessageChildTestFailure(test, fixture, &builder, fixture.inputsHash, input, changedHash, nil, ErrIntegrity)
	}
	for _, selected := range []DocMessageInput{fixture.inputs[1], {}} {
		builder := docEventDisplayBuilder{ctx: context.Background(), load: fixture.load}
		eventMessageChildTestFailure(test, fixture, &builder, fixture.inputsHash, selected, hash, nil, ErrIntegrity)
	}
	builder := docEventDisplayBuilder{ctx: context.Background(), load: fixture.load}
	eventMessageChildTestFailure(test, fixture, &builder, HashContent([]byte("other inputs")), input, hash, nil, ErrIntegrity)
}

func TestDocEventMessageChildRejectsForeignTypedStateAndGeneration(test *testing.T) {
	fixture := newEventMessageChildTestFixture(test, []byte(`{"kind":"message","blocks":[{"type":"text","text":"same"},{"type":"text","text":"same"},{"type":"tool","name":"same"},{"type":"tool","name":"same"}]}`))
	input := fixture.inputs[0]
	_, textTask := fixture.checkpoint(test, input, "text")
	_, markdownTask := fixture.checkpoint(test, input, "markdown")
	_, foreignText := fixture.checkpoint(test, fixture.inputs[1], "text")
	_, foreignMarkdown := fixture.checkpoint(test, fixture.inputs[1], "markdown")
	_, nameTask := fixture.checkpoint(test, fixture.inputs[2], "name")
	_, placeholderTask := fixture.checkpoint(test, fixture.inputs[2], "placeholder")
	_, foreignName := fixture.checkpoint(test, fixture.inputs[3], "name")
	_, foreignPlaceholder := fixture.checkpoint(test, fixture.inputs[3], "placeholder")
	wrongMarkdown, err := NewDocMarkdownPlanState(DocMarkdownPlanInput{Hash: HashContent([]byte("different")), Bytes: 9})
	if err != nil {
		test.Fatal(err)
	}
	wrongMarkdownHash := fixture.store(eventMessageChildTestJSON(test, wrongMarkdown))
	wrongMarkdown.Input.Hash = HashContent([]byte("same"))
	wrongLengthHash := fixture.store(eventMessageChildTestJSON(test, wrongMarkdown))
	source, err := input.TextSource()
	if err != nil {
		test.Fatal(err)
	}
	completedText := buildDecodedTextSourceFixture(test, fixture.raw, *source)
	completedTextHash := fixture.store(eventMessageChildTestJSON(test, completedText.state))
	completedMarkdown, err := DecodeDocMarkdownPlanState(markdownTask.Active, fixture.objects[markdownTask.Active])
	if err != nil {
		test.Fatal(err)
	}
	generation, err := DecodeDocDecodedTextGeneration(markdownTask.Text, fixture.objects[markdownTask.Text])
	if err != nil {
		test.Fatal(err)
	}
	for !completedMarkdown.Done() {
		step, err := StepDocDecodedMarkdownPlan(context.Background(), generation, completedMarkdown, fixture.load)
		if err != nil {
			test.Fatal(err)
		}
		for _, object := range step.Objects {
			fixture.store(object.Bytes)
		}
		completedMarkdown = step.State
	}
	completedMarkdownHash := fixture.store(eventMessageChildTestJSON(test, completedMarkdown))
	for _, sample := range []struct {
		name   string
		input  DocMessageInput
		task   docEventMessageChildTask
		mutate func(*docEventMessageChildTask)
	}{
		{"foreign-decode-token", input, textTask, func(task *docEventMessageChildTask) { task.Active = foreignText.Active }},
		{"completed-decode-state", input, textTask, func(task *docEventMessageChildTask) { task.Active = completedTextHash }},
		{"foreign-generation-token", input, markdownTask, func(task *docEventMessageChildTask) { task.Text = foreignMarkdown.Text }},
		{"wrong-markdown-input", input, markdownTask, func(task *docEventMessageChildTask) { task.Active = wrongMarkdownHash }},
		{"wrong-markdown-length", input, markdownTask, func(task *docEventMessageChildTask) { task.Active = wrongLengthHash }},
		{"completed-markdown-state", input, markdownTask, func(task *docEventMessageChildTask) { task.Active = completedMarkdownHash }},
		{"foreign-type-token", fixture.inputs[2], nameTask, func(task *docEventMessageChildTask) { task.Type = foreignName.Type }},
		{"foreign-name-state", fixture.inputs[2], nameTask, func(task *docEventMessageChildTask) { task.Active = foreignName.Active }},
		{"foreign-name-generation", fixture.inputs[2], placeholderTask, func(task *docEventMessageChildTask) { task.Name = foreignPlaceholder.Name }},
		{"foreign-placeholder", fixture.inputs[2], placeholderTask, func(task *docEventMessageChildTask) { task.Active = foreignPlaceholder.Active }},
		{"missing-type-generation", fixture.inputs[2], nameTask, func(task *docEventMessageChildTask) { task.Type = "" }},
		{"premature-name-generation", fixture.inputs[2], nameTask, func(task *docEventMessageChildTask) { task.Name = placeholderTask.Name }},
		{"missing-name-generation", fixture.inputs[2], placeholderTask, func(task *docEventMessageChildTask) { task.Name = "" }},
	} {
		test.Run(sample.name, func(test *testing.T) {
			task := sample.task
			sample.mutate(&task)
			hash := fixture.store(eventMessageChildTestJSON(test, task))
			builder := docEventDisplayBuilder{ctx: context.Background(), load: fixture.load}
			eventMessageChildTestFailure(test, fixture, &builder, fixture.inputsHash, sample.input, hash, nil, ErrIntegrity)
		})
	}
	for _, original := range []docEventMessageChildTask{textTask, markdownTask, nameTask, placeholderTask} {
		selected := input
		if original.Block == 2 {
			selected = fixture.inputs[2]
		}
		for _, raw := range [][]byte{append(bytes.Clone(fixture.objects[original.Active]), ' '), bytes.Repeat([]byte(" "), MaxDocDisplayStateBytes+1)} {
			task := original
			task.Active = fixture.store(raw)
			hash := fixture.store(eventMessageChildTestJSON(test, task))
			builder := docEventDisplayBuilder{ctx: context.Background(), load: fixture.load}
			eventMessageChildTestFailure(test, fixture, &builder, fixture.inputsHash, selected, hash, nil, ErrIntegrity)
		}
	}
}

func TestDocEventMessageChildErrorsCancellationAndStagingRollback(test *testing.T) {
	fixture := newEventMessageChildTestFixture(test, []byte(`{"kind":"message","blocks":[{"type":"text","text":"PUBLIC_BODY_MARKER"},{}]}`))
	input := fixture.inputs[0]
	hash := fixture.start(test, input)
	for _, sample := range []struct {
		name string
		load func(context.Context, int, int) ([]byte, error)
		err  error
	}{
		{"missing-loader", nil, ErrIntegrity},
		{"raw-error", func(context.Context, int, int) ([]byte, error) { return nil, ErrNotFound }, ErrNotFound},
		{"short-raw", func(context.Context, int, int) ([]byte, error) { return nil, nil }, ErrIntegrity},
		{"invalid-token", func(_ context.Context, _ int, length int) ([]byte, error) {
			return bytes.Repeat([]byte("x"), length), nil
		}, ErrIntegrity},
	} {
		test.Run(sample.name, func(test *testing.T) {
			builder := docEventDisplayBuilder{ctx: context.Background(), load: fixture.load}
			eventMessageChildTestFailure(test, fixture, &builder, fixture.inputsHash, input, hash, sample.load, sample.err)
		})
	}
	for _, phase := range []string{"before-start", "raw-load", "child-load", "finished-placeholder"} {
		test.Run(phase, func(test *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			selected, childHash := input, hash
			if phase == "finished-placeholder" {
				selected = fixture.inputs[1]
				childHash = fixture.start(test, selected)
			}
			builder := docEventDisplayBuilder{ctx: ctx, load: fixture.load}
			if phase == "before-start" {
				cancel()
				started, err := builder.startMessageChild(fixture.inputsHash, selected)
				if !errors.Is(err, context.Canceled) || started != "" || len(builder.objects) != 0 {
					test.Fatal("cancelled start exposed output", err)
				}
			} else if phase == "child-load" || phase == "finished-placeholder" {
				builder.load = func(ctx context.Context, hash ContentHash) ([]byte, error) {
					raw, err := fixture.load(ctx, hash)
					if phase == "child-load" || hash != childHash {
						cancel()
					}
					return raw, err
				}
			}
			loadRaw := func(_ context.Context, from, length int) ([]byte, error) {
				cancel()
				return bytes.Clone(fixture.raw[from : from+length]), nil
			}
			eventMessageChildTestFailure(test, fixture, &builder, fixture.inputsHash, selected, childHash, loadRaw, context.Canceled)
		})
	}
	loadRaw := func(_ context.Context, from, length int) ([]byte, error) {
		return bytes.Clone(fixture.raw[from : from+length]), nil
	}
	builder := docEventDisplayBuilder{ctx: context.Background(), load: fixture.load, bytes: MaxDocEventDisplayStepBytes - 1}
	eventMessageChildTestFailure(test, fixture, &builder, fixture.inputsHash, input, hash, loadRaw, ErrIntegrity)
	builder = docEventDisplayBuilder{ctx: context.Background(), load: fixture.load, objects: make([]DocRangeObject, MaxDocEventDisplayStepObjects-1)}
	eventMessageChildTestFailure(test, fixture, &builder, fixture.inputsHash, input, hash, loadRaw, ErrIntegrity)
	for _, selected := range []DocMessageInput{input, fixture.inputs[1]} {
		builder := docEventDisplayBuilder{ctx: context.Background(), load: fixture.load, objects: make([]DocRangeObject, MaxDocEventDisplayStepObjects-1)}
		started, err := builder.startMessageChild(fixture.inputsHash, selected)
		if !errors.Is(err, ErrIntegrity) || started != "" || len(builder.objects) != MaxDocEventDisplayStepObjects-1 || builder.bytes != 0 {
			test.Fatal("failed metadata staging retained the active object", err)
		}
	}
	for _, load := range []func(context.Context, ContentHash) ([]byte, error){
		func(context.Context, ContentHash) ([]byte, error) { return nil, ErrNotFound },
		func(context.Context, ContentHash) ([]byte, error) { return []byte("wrong hash"), nil },
	} {
		builder := docEventDisplayBuilder{ctx: context.Background(), load: load}
		expected := ErrIntegrity
		if _, err := load(context.Background(), hash); err != nil {
			expected = err
		}
		eventMessageChildTestFailure(test, fixture, &builder, fixture.inputsHash, input, hash, loadRaw, expected)
	}
}
