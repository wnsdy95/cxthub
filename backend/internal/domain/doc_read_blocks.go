package domain

import (
	"context"
	"strings"
)

// ReadIndexBlockEvents bounds repeated location writes for a growing capture.
// These are derived index blocks, independent of archival byte-chunk boundaries.
const ReadIndexBlockEvents = 128

type DocReadBlock struct {
	Hash       ContentHash
	FirstEvent int
	Offset     int
	Count      int
	plan       DocReadPlan
}

// Blocks share only exact ordered canonical event identities under one read
// projection/CIR version. Document-relative positions live on each reference;
// block-local positions do not change when a preceding event's size changes.
func (p DocReadPlan) Blocks() []DocReadBlock {
	var blocks []DocReadBlock
	for start := 0; start < len(p.index.Events); start += ReadIndexBlockEvents {
		end := min(start+ReadIndexBlockEvents, len(p.index.Events))
		events := p.index.Events[start:end]
		var key strings.Builder
		key.WriteString("cxt-read-block-v1\n" + p.index.Envelope.CIRVersion + "\n")
		for _, event := range events {
			key.WriteString(string(event.Hash) + "\n")
		}
		index := p.index
		index.Events = append([]DocEventIndex(nil), events...)
		for i := range index.Events {
			index.Events[i].Index = i
			index.Events[i].Offset -= events[0].Offset
		}
		blocks = append(blocks, DocReadBlock{Hash: HashContent([]byte(key.String())), FirstEvent: start, Offset: events[0].Offset, Count: end - start,
			plan: DocReadPlan{index: index, stream: p.stream, source: p.source, bodies: p.bodies[start:end], sequencesKnown: p.sequencesKnown}})
	}
	return blocks
}

func (p DocReadPlan) Envelope() CIREnvelope {
	out := p.index.Envelope
	out.SourceModels = append([]string(nil), out.SourceModels...)
	return out
}
func (p DocReadPlan) EventCount() int             { return len(p.index.Events) }
func (b DocReadBlock) EventHashes() []ContentHash { return b.plan.EventHashes() }
func (b DocReadBlock) Build(known map[ContentHash]bool) (DocReadIndex, error) {
	return b.BuildContext(context.Background(), known)
}

func (b DocReadBlock) BuildContext(ctx context.Context, known map[ContentHash]bool) (DocReadIndex, error) {
	return b.plan.BuildContext(ctx, known)
}

func (b DocReadBlock) VisitContext(ctx context.Context, known map[ContentHash]bool, visit func(DocEventIndex) error) error {
	return b.plan.visit(ctx, known, false, visit)
}
