//go:build postgres

package store

import (
	"context"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type docRangeScopeTracePG struct {
	docRangeGenerationTracer
	isolation  atomic.Int64
	identities atomic.Int64
}

func (trace *docRangeScopeTracePG) TraceQueryStart(ctx context.Context, connection *pgx.Conn, query pgx.TraceQueryStartData) context.Context {
	if strings.Contains(query.SQL, "current_setting('transaction_isolation')") {
		trace.isolation.Add(1)
	}
	if query.SQL == docRangeJobIdentitySQL {
		trace.identities.Add(1)
	}
	return trace.docRangeGenerationTracer.TraceQueryStart(ctx, connection, query)
}

func (trace *docRangeScopeTracePG) facts() [2]int64 {
	return [2]int64{trace.descriptors.Load(), trace.objects.Load()}
}

func TestDocRangeScopeDescriptorIdentityCopies(test *testing.T) {
	identity := domain.DocumentIdentityLegacy
	reader := &postgresDocRangeGenerationRead{generation: domain.DocRangeGeneration{SourceIdentity: &identity, Envelope: []byte(`{"source":"original"}`)}}
	expected := reader.Descriptor()
	changed := reader.Descriptor()
	*changed.SourceIdentity = domain.DocumentIdentityRootV1
	changed.Envelope[0] = '!'
	if !reflect.DeepEqual(reader.Descriptor(), expected) || identity != domain.DocumentIdentityLegacy {
		test.Fatal("shared descriptor leaked its identity pointer or envelope bytes")
	}
}
