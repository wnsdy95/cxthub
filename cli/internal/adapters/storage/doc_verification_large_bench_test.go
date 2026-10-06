package storage

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

func BenchmarkColdLargeFirstEventBatch(b *testing.B) {
	for _, shared := range []bool{false, true} {
		name := "unrelated"
		if shared {
			name = "shared-prefix"
		}
		b.Run(name, func(b *testing.B) {
			fixture := NewFileStore(b.TempDir())
			var ids []domain.ContentHash
			for n := 0; n < 8; n++ {
				token := fmt.Sprintf("text%03d ", n)
				if shared {
					token = "context "
				}
				doc := domain.CIRDocument{Envelope: domain.Envelope{CIRVersion: "1", GitBranch: fmt.Sprint(n)}, Events: []domain.Event{{Kind: domain.EventMessage, Seq: 0, Role: "user", Blocks: []domain.ContentBlock{{Type: "text", Text: strings.Repeat(token, 1<<20)}}}}}
				if shared {
					doc.Events = append(doc.Events, domain.Event{Kind: domain.EventTurn, Seq: 1, Role: fmt.Sprint(n)})
				}
				id, err := fixture.PutDoc(context.Background(), domain.SessionDoc{CIR: doc})
				if err != nil {
					b.Fatal(err)
				}
				ids = append(ids, id)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				store := NewFileStore(fixture.repoRoot)
				for _, id := range ids {
					if err := store.VerifyStoredDoc(context.Background(), id); err != nil {
						b.Fatal(err)
					}
				}
			}
		})
	}
}
