package domain

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
)

// Reproduce the former read-plan allocations for a before/after comparison:
// Bytes materialized the full canonical string, then Unmarshal copied every
// event into a RawMessage array, before hashing each event for its location.
func benchmarkLegacyReadPlan(doc VerifiedSessionDoc) (DocReadIndex, []json.RawMessage, error) {
	env, bodies, err := splitCanonicalDocBytes(doc.Bytes())
	if err != nil {
		return DocReadIndex{}, nil, err
	}
	index := DocReadIndex{Version: 1, Hash: doc.Hash(), Events: make([]DocEventIndex, 0, len(bodies))}
	if err := json.Unmarshal(env, &index.Envelope); err != nil {
		return DocReadIndex{}, nil, err
	}
	offset := 0
	for i, body := range bodies {
		index.Events = append(index.Events, DocEventIndex{Index: i, Offset: offset, Length: len(body), Hash: HashContent(body)})
		offset += len(body) + 1
	}
	return index, bodies, nil
}

// Covers the actual publication preparation APIs, including their required
// owned chunk output. The baseline reproduces bounded current-chunk hashing,
// full assembly, Verify, PlanDocChunks(Bytes), and the former read planner.
// Both variants compute the full canonical SHA; these are allocation/scan
// improvements, not delta document-hash computation.
func BenchmarkCanonicalChunkPipeline(b *testing.B) {
	for _, count := range []int{1, 256} {
		b.Run(fmt.Sprintf("8MiB/events=%d", count), func(b *testing.B) {
			base := canonicalAdmissionSizedDoc(b, count, 8<<20)
			appendedEvent, err := canonicalJSON(CIREvent{Kind: EventMessage, Role: RoleAssistant, Seq: count})
			if err != nil {
				b.Fatal(err)
			}
			raw := append(append(append([]byte{}, base[:len(base)-2]...), ','), appendedEvent...)
			raw = append(raw, ']', '}')
			input, ok := PlanDocChunks(raw)
			if !ok {
				b.Fatal("missing fixture chunks")
			}
			want := HashContent(raw)
			for _, name := range []string{"assembled-cold", "chunks-cold", "assembled-warm-append", "chunks-warm-append"} {
				b.Run(name, func(b *testing.B) {
					var shared CanonicalDocVerifier
					warm := name == "assembled-warm-append" || name == "chunks-warm-append"
					chunked := name == "chunks-cold" || name == "chunks-warm-append"
					if warm {
						if _, err := shared.Verify(context.Background(), HashContent(base), base); err != nil {
							b.Fatal(err)
						}
					}
					b.SetBytes(int64(len(raw)))
					b.ReportAllocs()
					b.ResetTimer()
					for b.Loop() {
						v := &shared
						if !warm {
							v = &CanonicalDocVerifier{}
						}
						var doc VerifiedSessionDoc
						if chunked {
							doc, err = v.VerifyChunks(context.Background(), want, input.Manifest, chunkVerifierLoad(input))
						} else {
							parts := make([][]byte, 0, len(input.Order))
							for _, h := range input.Order {
								body := input.Bodies[h]
								if HashContent(body) != h {
									b.Fatal("chunk corrupted")
								}
								parts = append(parts, body)
							}
							assembled, assembleErr := AssembleDocChunks(input.Manifest, parts, want)
							if assembleErr != nil {
								b.Fatal(assembleErr)
							}
							doc, err = v.Verify(context.Background(), want, assembled)
						}
						if err != nil {
							b.Fatal(err)
						}
						if chunked {
							if _, ok := doc.ChunkPlan(); !ok {
								b.Fatal("missing output chunks")
							}
							if plan, err := doc.PlanReadIndexContext(context.Background()); err != nil || plan.EventCount() != count+1 {
								b.Fatal("invalid read plan", err)
							}
						} else {
							if _, ok := PlanDocChunks(doc.Bytes()); !ok {
								b.Fatal("missing output chunks")
							}
							if index, _, err := benchmarkLegacyReadPlan(doc); err != nil || len(index.Events) != count+1 {
								b.Fatal("invalid legacy read plan", err)
							}
						}
					}
				})
			}
		})
	}
}
