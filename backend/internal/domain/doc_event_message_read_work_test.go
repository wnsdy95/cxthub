package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
)

func eventMessageReadWorkBody() []byte {
	var raw bytes.Buffer
	raw.WriteString(`{"kind":"message","blocks":[`)
	for index := range DocMessageInputPageEntries + 1 {
		if index > 0 {
			raw.WriteByte(',')
		}
		if index%7 == 0 {
			fmt.Fprintf(&raw, `{"type":"image","name":"asset %d","opaque":"private"}`, index)
		} else {
			fmt.Fprintf(&raw, `{"type":"text","text":"**block %d 한🙂**","opaque":"private"}`, index)
		}
	}
	raw.WriteString(`]}`)
	return raw.Bytes()
}

func TestDocEventMessagePreparationReadWorkExactTrace(test *testing.T) {
	fixture := newEventMessageFixture(test, eventMessageReadWorkBody())
	type stepTrace struct {
		Prior       ContentHash
		Preparation json.RawMessage
		Objects     []ContentHash
		Loads       []ContentHash
		Raw         []DocFindSpan
	}
	var trace []stepTrace
	for fixture.state.Phase != "completed" {
		if len(trace) > 1024 {
			test.Fatal("message fixture stopped making bounded progress")
		}
		var observation stepTrace
		step, err := StepDocEventDisplay(context.Background(), fixture.state,
			func(ctx context.Context, offset, length int) ([]byte, error) {
				observation.Raw = append(observation.Raw, DocFindSpan{From: offset, To: offset + length})
				return fixture.loadRaw(ctx, offset, length)
			}, func(ctx context.Context, hash ContentHash) ([]byte, error) {
				observation.Loads = append(observation.Loads, hash)
				return fixture.stored(ctx, hash)
			})
		if err != nil {
			test.Fatal("preparation trace failed", fixture.state.Phase, err)
		}
		observation.Prior = step.Prior()
		observation.Preparation, err = step.Preparation().Encode()
		if err != nil {
			test.Fatal(err)
		}
		for _, object := range step.Objects() {
			if HashContent(object.Bytes) != object.Hash {
				test.Fatal("derived object hash mismatch")
			}
			observation.Objects = append(observation.Objects, object.Hash)
			fixture.objects[object.Hash] = bytes.Clone(object.Bytes)
		}
		fixture.state = fixture.restore(test, step.Preparation())
		trace = append(trace, observation)
	}
	generation := fixture.finish(test)
	fixture.pages(test, generation, fixture.oracle(test))
	raw, err := json.Marshal(struct {
		Steps      []stepTrace
		Generation DocDisplayProjectionGeneration
	}{Steps: trace, Generation: generation})
	if err != nil {
		test.Fatal(err)
	}
	fingerprint := HashContent(raw)
	const expected ContentHash = "sha256:02212b94e12a723c840184993e7dfda9d6f324e07b9a14189bbf016998bd2874"
	if fingerprint != expected {
		test.Fatal("preparation changed exact physical reads, byte spans, object order, checkpoints or generation", fingerprint, expected)
	}
	test.Logf("fingerprint=%s steps=%d", fingerprint, len(trace))
}

func messagePreparationReadWorkFixture(test *testing.T) *eventMessageFixture {
	test.Helper()
	fixture := newEventMessageFixture(test, eventMessageReadWorkBody())
	for fixture.state.Phase != "message_plan" {
		if err := fixture.advance(test); err != nil {
			test.Fatal(err)
		}
	}
	return fixture
}

func TestDocEventMessageParentReusesInputProof(test *testing.T) {
	fixture := messagePreparationReadWorkFixture(test)
	reader := docMessageInputReader{}
	builder := docEventDisplayBuilder{ctx: context.Background(), state: fixture.state, load: fixture.stored}
	if err := builder.messagePlan(fixture.loadRaw, &reader); err != nil {
		test.Fatal(err)
	}
	if len(reader.inputs) != DocMessageInputPageEntries {
		test.Fatal("actual parent did not retain its bounded input-page proof", len(reader.inputs))
	}
	proof := &reader.inputs[0]
	if err := builder.messagePlan(fixture.loadRaw, &reader); err != nil || &reader.inputs[0] != proof {
		test.Fatal("parent transition decoded the same authenticated page again", err)
	}
	independent := docMessageInputReader{}
	restarted := docEventDisplayBuilder{ctx: context.Background(), state: fixture.state, load: fixture.stored}
	if err := restarted.messagePlan(fixture.loadRaw, &independent); err != nil || len(independent.inputs) == 0 || &independent.inputs[0] == proof {
		test.Fatal("independent parent retained another step's parsed proof", err)
	}
	if err := restarted.messagePlan(fixture.loadRaw, nil); !errors.Is(err, ErrIntegrity) {
		test.Fatal("nil private reader accepted", err)
	}
}

func TestDocEventMessageWarmInputProofReauthenticates(test *testing.T) {
	fixture := messagePreparationReadWorkFixture(test)
	inputs := fixture.inputs(test, fixture.state)
	pageHash, err := ResolveDocRangeLeaf(context.Background(), inputs.Pages, 0, fixture.stored)
	if err != nil {
		test.Fatal(err)
	}
	prior, err := fixture.state.Encode()
	if err != nil {
		test.Fatal(err)
	}
	loadFailure := errors.New("selected page unavailable")
	for _, scenario := range []struct {
		name string
		want error
	}{
		{"load", loadFailure},
		{"corrupt", ErrIntegrity},
		{"cancel", context.Canceled},
		{"cancel-load", context.Canceled},
		{"cancel-corrupt", context.Canceled},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			reads := 0
			step, err := StepDocEventDisplay(ctx, fixture.state, fixture.loadRaw,
				func(ctx context.Context, hash ContentHash) ([]byte, error) {
					if hash == pageHash {
						reads++
						if reads == 2 {
							switch scenario.name {
							case "cancel", "cancel-load", "cancel-corrupt":
								cancel()
							}
							switch scenario.name {
							case "load", "cancel-load":
								return nil, loadFailure
							case "corrupt", "cancel-corrupt":
								return []byte("corrupt"), nil
							}
						}
					}
					return fixture.stored(ctx, hash)
				})
			if !errors.Is(err, scenario.want) || reads != 2 {
				test.Fatal("warm proof bypassed fresh bytes, errors or cancellation", reads, err)
			}
			eventMessageZeroStep(test, step)
			after, err := fixture.state.Encode()
			if err != nil || !bytes.Equal(prior, after) {
				test.Fatal("failed warm hit changed predecessor", err)
			}
		})
	}
}
