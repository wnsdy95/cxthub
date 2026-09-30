package outbound

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// SyncDiagnosticStage is a closed set of passive sync observations.
type SyncDiagnosticStage string

const (
	SyncStageHookReplay       SyncDiagnosticStage = "hook_replay"
	SyncStagePush             SyncDiagnosticStage = "push"
	SyncStageRetention        SyncDiagnosticStage = "retention"
	SyncStagePreparation      SyncDiagnosticStage = "preparation"
	SyncStageDocumentRead     SyncDiagnosticStage = "document_read"
	SyncStageCanonical        SyncDiagnosticStage = "canonical"
	SyncStageChunkPlan        SyncDiagnosticStage = "chunk_plan"
	SyncStageRequestMarshal   SyncDiagnosticStage = "request_marshal"
	SyncStageRequestSetup     SyncDiagnosticStage = "request_setup"
	SyncStageTokenLookup      SyncDiagnosticStage = "token_lookup"
	SyncStageHTTPDo           SyncDiagnosticStage = "http_do"
	SyncStageHTTPBody         SyncDiagnosticStage = "http_body"
	SyncStageHTTPGotConn      SyncDiagnosticStage = "http_got_conn"
	SyncStageHTTPWroteRequest SyncDiagnosticStage = "http_wrote_request"
	SyncStageHTTPFirstByte    SyncDiagnosticStage = "http_first_byte"
	SyncStageUnknown          SyncDiagnosticStage = "unknown"
)

type SyncDiagnosticRole string

const (
	SyncRoleInventory      SyncDiagnosticRole = "inventory"
	SyncRoleDocumentChunks SyncDiagnosticRole = "document_chunks"
	SyncRoleOther          SyncDiagnosticRole = "other"
)

// SyncDiagnosticCounts accepts numeric metadata only. Negative values become zero.
type SyncDiagnosticCounts struct {
	Snapshots           int   `json:"snapshots"`
	Documents           int   `json:"documents"`
	Chunks              int   `json:"chunks"`
	Bytes               int   `json:"bytes"`
	HTTPStatus          int   `json:"http_status"`
	ClientTimeoutMillis int64 `json:"client_timeout_millis"`
}

type SyncDiagnosticContextState string

const (
	SyncContextActive   SyncDiagnosticContextState = "active"
	SyncContextDeadline SyncDiagnosticContextState = "deadline"
	SyncContextCanceled SyncDiagnosticContextState = "canceled"
	SyncContextOther    SyncDiagnosticContextState = "other"
)

type SyncDiagnosticErrorClass string

const (
	SyncDiagnosticSuccess          SyncDiagnosticErrorClass = "success"
	SyncDiagnosticDeadlineBeforeDo SyncDiagnosticErrorClass = "deadline_before_do"
	SyncDiagnosticDeadline         SyncDiagnosticErrorClass = "deadline"
	SyncDiagnosticCanceled         SyncDiagnosticErrorClass = "canceled"
	SyncDiagnosticTimeoutActive    SyncDiagnosticErrorClass = "timeout_active"
	SyncDiagnosticError            SyncDiagnosticErrorClass = "error"
)

// SyncDiagnosticEvent contains no user text, error payloads, or retained contexts.
// ElapsedMillis is measured at the start, relative to collector creation. A nil
// StartRemainingMillis means the original caller has no deadline;
// a negative value means its deadline had already passed at the start.
type SyncDiagnosticEvent struct {
	OperationID          string                     `json:"operation_id"`
	Sequence             uint64                     `json:"sequence"`
	Stage                SyncDiagnosticStage        `json:"stage"`
	Role                 SyncDiagnosticRole         `json:"role"`
	Attempt              int                        `json:"attempt"`
	Document             int                        `json:"document"`
	Request              uint64                     `json:"request"`
	Counts               SyncDiagnosticCounts       `json:"counts"`
	ElapsedMillis        int64                      `json:"elapsed_millis"`
	DurationMillis       int64                      `json:"duration_millis"`
	StartRemainingMillis *int64                     `json:"start_remaining_millis"`
	EndRemainingMillis   *int64                     `json:"end_remaining_millis"`
	StartContextState    SyncDiagnosticContextState `json:"start_context_state"`
	EndContextState      SyncDiagnosticContextState `json:"end_context_state"`
	StartCallerState     SyncDiagnosticContextState `json:"start_caller_state"`
	EndCallerState       SyncDiagnosticContextState `json:"end_caller_state"`
	Outcome              SyncDiagnosticErrorClass   `json:"outcome"`
}

// SyncDiagnosticStageTotals survive event eviction. Counts are stage-local sums;
// callers should record bytes only at the one stage that owns the measurement.
// Status codes and configured client timeouts are not additive totals.
type SyncDiagnosticStageTotals struct {
	Stage          SyncDiagnosticStage `json:"stage"`
	Calls          uint64              `json:"calls"`
	Failures       uint64              `json:"failures"`
	DurationMillis int64               `json:"duration_millis"`
	Snapshots      int64               `json:"snapshots"`
	Documents      int64               `json:"documents"`
	Chunks         int64               `json:"chunks"`
	Bytes          int64               `json:"bytes"`
}

// SyncDiagnosticsReport is an independent copy, ordered by completion sequence.
type SyncDiagnosticsReport struct {
	OperationID   string                      `json:"operation_id"`
	ElapsedMillis int64                       `json:"elapsed_millis"`
	Totals        []SyncDiagnosticStageTotals `json:"totals"`
	Events        []SyncDiagnosticEvent       `json:"events"`
	DroppedCount  uint64                      `json:"dropped_count"`
	HasFailures   bool                        `json:"has_failures"`
}

// SyncDiagnostics is optional process-local storage. It never performs I/O.
// Construct it with NewSyncDiagnostics; its zero value also uses the default limit.
type SyncDiagnostics struct {
	mu          sync.Mutex
	operationID string
	started     time.Time
	limit       int
	events      []SyncDiagnosticEvent
	head        int
	sequence    uint64
	requests    uint64
	dropped     uint64
	hasFailures bool
	totals      [16]SyncDiagnosticStageTotals
}

var syncDiagnosticOperations atomic.Uint64

// NewSyncDiagnostics defaults to 64 events for nonpositive limits and caps at 128.
func NewSyncDiagnostics(limit int) *SyncDiagnostics {
	if limit <= 0 {
		limit = 64
	}
	if limit > 128 {
		limit = 128
	}
	d := &SyncDiagnostics{limit: limit}
	d.initializeLocked()
	return d
}

func (d *SyncDiagnostics) initializeLocked() {
	if d.operationID != "" {
		return
	}
	d.operationID = "sync-" + strconv.FormatUint(syncDiagnosticOperations.Add(1), 10)
	d.started = time.Now()
	if d.limit == 0 {
		d.limit = 64
	}
	d.events = make([]SyncDiagnosticEvent, 0, d.limit)
}

// HasFailures remains true even after a failed event is evicted.
func (d *SyncDiagnostics) HasFailures() bool {
	if d == nil {
		return false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.hasFailures
}

func (d *SyncDiagnostics) Snapshot() SyncDiagnosticsReport {
	if d == nil {
		return SyncDiagnosticsReport{Events: []SyncDiagnosticEvent{}, Totals: []SyncDiagnosticStageTotals{}}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.initializeLocked()
	r := SyncDiagnosticsReport{
		OperationID: d.operationID, Events: make([]SyncDiagnosticEvent, len(d.events)),
		DroppedCount: d.dropped, HasFailures: d.hasFailures,
		ElapsedMillis: time.Since(d.started).Milliseconds(), Totals: make([]SyncDiagnosticStageTotals, 0, len(d.totals)),
	}
	for i := range r.Events {
		r.Events[i] = d.events[(d.head+i)%len(d.events)]
		if remaining := r.Events[i].StartRemainingMillis; remaining != nil {
			value := *remaining
			r.Events[i].StartRemainingMillis = &value
		}
		if remaining := r.Events[i].EndRemainingMillis; remaining != nil {
			value := *remaining
			r.Events[i].EndRemainingMillis = &value
		}
	}
	for _, total := range d.totals {
		if total.Calls > 0 {
			r.Totals = append(r.Totals, total)
		}
	}
	return r
}

type syncDiagnosticCollectorKey struct{}
type syncDiagnosticScopeKey struct{}
type syncDiagnosticRequestKey struct{}

type syncDiagnosticBinding struct {
	collector *SyncDiagnostics
	caller    context.Context
}

type syncDiagnosticScope struct {
	role     SyncDiagnosticRole
	attempt  int
	document int
}

type syncDiagnosticRequest struct {
	collector *SyncDiagnostics
	number    uint64
}

// WithSyncDiagnostics preserves cancellation and deadlines. The attached context
// defines the original caller budget, including for subsequently derived timeouts.
// Attaching nil disables collection in this context and its descendants.
func WithSyncDiagnostics(ctx context.Context, collector *SyncDiagnostics) context.Context {
	return context.WithValue(ctx, syncDiagnosticCollectorKey{}, syncDiagnosticBinding{collector: collector, caller: ctx})
}

func syncDiagnosticsBinding(ctx context.Context) syncDiagnosticBinding {
	if ctx == nil {
		return syncDiagnosticBinding{}
	}
	binding, _ := ctx.Value(syncDiagnosticCollectorKey{}).(syncDiagnosticBinding)
	return binding
}

// SyncDiagnosticsFromContext returns nil when collection is disabled.
func SyncDiagnosticsFromContext(ctx context.Context) *SyncDiagnostics {
	return syncDiagnosticsBinding(ctx).collector
}

// WithSyncDiagnosticRole preserves attempt, document, and request numbers.
func WithSyncDiagnosticRole(ctx context.Context, role SyncDiagnosticRole) context.Context {
	if SyncDiagnosticsFromContext(ctx) == nil {
		return ctx
	}
	scope, _ := ctx.Value(syncDiagnosticScopeKey{}).(syncDiagnosticScope)
	scope.role = sanitizeSyncDiagnosticRole(role)
	return context.WithValue(ctx, syncDiagnosticScopeKey{}, scope)
}

// WithSyncDiagnosticDocument preserves role, attempt, and request numbers.
func WithSyncDiagnosticDocument(ctx context.Context, ordinal int) context.Context {
	if SyncDiagnosticsFromContext(ctx) == nil {
		return ctx
	}
	scope, _ := ctx.Value(syncDiagnosticScopeKey{}).(syncDiagnosticScope)
	scope.document = max(0, ordinal)
	return context.WithValue(ctx, syncDiagnosticScopeKey{}, scope)
}

// WithSyncDiagnosticAttempt preserves role, document, and request numbers.
func WithSyncDiagnosticAttempt(ctx context.Context, attempt int) context.Context {
	if SyncDiagnosticsFromContext(ctx) == nil {
		return ctx
	}
	scope, _ := ctx.Value(syncDiagnosticScopeKey{}).(syncDiagnosticScope)
	scope.attempt = max(0, attempt)
	return context.WithValue(ctx, syncDiagnosticScopeKey{}, scope)
}

func WithSyncDiagnosticScope(ctx context.Context, role SyncDiagnosticRole, attempt, document int) context.Context {
	if syncDiagnosticsBinding(ctx).collector == nil {
		return ctx
	}
	return context.WithValue(ctx, syncDiagnosticScopeKey{}, syncDiagnosticScope{
		role: sanitizeSyncDiagnosticRole(role), attempt: max(0, attempt), document: max(0, document),
	})
}

// NextSyncDiagnosticRequest returns a derived context with the next operation-wide
// request number. Existing contexts and their request numbers remain unchanged.
func NextSyncDiagnosticRequest(ctx context.Context) context.Context {
	d := syncDiagnosticsBinding(ctx).collector
	if d == nil {
		return ctx
	}
	d.mu.Lock()
	d.requests++
	request := d.requests
	d.mu.Unlock()
	return context.WithValue(ctx, syncDiagnosticRequestKey{}, syncDiagnosticRequest{collector: d, number: request})
}

type activeSyncDiagnostic struct {
	once      sync.Once
	collector *SyncDiagnostics
	ctx       context.Context
	caller    context.Context
	started   time.Time
	event     SyncDiagnosticEvent
}

// BeginSyncDiagnostic returns an exactly-once completion callback, safe for
// concurrent calls. Only the winning call's error classification is recorded.
func BeginSyncDiagnostic(ctx context.Context, stage SyncDiagnosticStage, counts SyncDiagnosticCounts) func(error) {
	active := beginSyncDiagnostic(ctx, stage, counts)
	if active == nil {
		return noopSyncDiagnostic
	}
	return active.finish
}

// BeginMeasuredSyncDiagnostic records final numeric counts and duration in one
// event. Completion and count assignment share the same exactly-once boundary.
func BeginMeasuredSyncDiagnostic(ctx context.Context, stage SyncDiagnosticStage) func(SyncDiagnosticCounts, error) {
	active := beginSyncDiagnostic(ctx, stage, SyncDiagnosticCounts{})
	if active == nil {
		return func(SyncDiagnosticCounts, error) {}
	}
	return func(counts SyncDiagnosticCounts, err error) {
		active.complete(err, false, &counts)
	}
}

func noopSyncDiagnostic(error) {}

func beginSyncDiagnostic(ctx context.Context, stage SyncDiagnosticStage, counts SyncDiagnosticCounts) *activeSyncDiagnostic {
	binding := syncDiagnosticsBinding(ctx)
	d := binding.collector
	if d == nil {
		return nil
	}
	d.mu.Lock()
	d.initializeLocked()
	operationID, operationStarted := d.operationID, d.started
	d.mu.Unlock()
	started := time.Now()
	scope, _ := ctx.Value(syncDiagnosticScopeKey{}).(syncDiagnosticScope)
	request, _ := ctx.Value(syncDiagnosticRequestKey{}).(syncDiagnosticRequest)
	if request.collector != d {
		request.number = 0
	}
	event := SyncDiagnosticEvent{
		OperationID: operationID, Stage: sanitizeSyncDiagnosticStage(stage),
		Role: sanitizeSyncDiagnosticRole(scope.role), Attempt: scope.attempt, Document: scope.document,
		Request: request.number, Counts: sanitizeSyncDiagnosticCounts(counts),
		ElapsedMillis:     started.Sub(operationStarted).Milliseconds(),
		StartContextState: syncDiagnosticContextState(ctx),
		StartCallerState:  syncDiagnosticContextState(binding.caller),
	}
	if deadline, ok := binding.caller.Deadline(); ok {
		remaining := deadline.Sub(started).Milliseconds()
		event.StartRemainingMillis = &remaining
	}
	return &activeSyncDiagnostic{collector: d, ctx: ctx, caller: binding.caller, started: started, event: event}
}

func (a *activeSyncDiagnostic) finish(err error) { a.complete(err, false, nil) }

func (a *activeSyncDiagnostic) complete(err error, instant bool, counts *SyncDiagnosticCounts) {
	a.once.Do(func() {
		if counts != nil {
			a.event.Counts = sanitizeSyncDiagnosticCounts(*counts)
		}
		ended := time.Now()
		if !instant {
			a.event.DurationMillis = ended.Sub(a.started).Milliseconds()
		}
		a.event.EndContextState = syncDiagnosticContextState(a.ctx)
		a.event.EndCallerState = syncDiagnosticContextState(a.caller)
		if deadline, ok := a.caller.Deadline(); ok {
			remaining := deadline.Sub(ended).Milliseconds()
			a.event.EndRemainingMillis = &remaining
		}
		a.event.Outcome = classifySyncDiagnosticError(a.event, err)
		// A retained completion callback must not retain contexts after completion.
		a.ctx, a.caller = nil, nil
		a.collector.record(a.event)
		a.collector = nil
	})
}

// RecordSyncDiagnostic records an instant observation with a zero duration.
func RecordSyncDiagnostic(ctx context.Context, stage SyncDiagnosticStage, counts SyncDiagnosticCounts, err error) {
	active := beginSyncDiagnostic(ctx, stage, counts)
	if active == nil {
		return
	}
	active.complete(err, true, nil)
}

func (d *SyncDiagnostics) record(event SyncDiagnosticEvent) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.sequence++
	event.Sequence = d.sequence
	d.hasFailures = d.hasFailures || event.Outcome != SyncDiagnosticSuccess
	total := &d.totals[syncDiagnosticStageIndex(event.Stage)]
	total.Stage = event.Stage
	total.Calls++
	if event.Outcome != SyncDiagnosticSuccess {
		total.Failures++
	}
	total.DurationMillis = syncDiagnosticAdd(total.DurationMillis, event.DurationMillis)
	total.Snapshots = syncDiagnosticAdd(total.Snapshots, int64(event.Counts.Snapshots))
	total.Documents = syncDiagnosticAdd(total.Documents, int64(event.Counts.Documents))
	total.Chunks = syncDiagnosticAdd(total.Chunks, int64(event.Counts.Chunks))
	total.Bytes = syncDiagnosticAdd(total.Bytes, int64(event.Counts.Bytes))
	if len(d.events) < d.limit {
		d.events = append(d.events, event)
		return
	}
	d.events[d.head] = event
	d.head = (d.head + 1) % d.limit
	d.dropped++
}

func syncDiagnosticContextState(ctx context.Context) SyncDiagnosticContextState {
	switch ctx.Err() {
	case nil:
		return SyncContextActive
	case context.DeadlineExceeded:
		return SyncContextDeadline
	case context.Canceled:
		return SyncContextCanceled
	default:
		return SyncContextOther
	}
}

func classifySyncDiagnosticError(event SyncDiagnosticEvent, err error) SyncDiagnosticErrorClass {
	if err == nil {
		return SyncDiagnosticSuccess
	}
	if event.Stage == SyncStageHTTPDo && event.StartContextState == SyncContextDeadline {
		return SyncDiagnosticDeadlineBeforeDo
	}
	if event.EndCallerState == SyncContextDeadline || event.StartCallerState == SyncContextDeadline {
		return SyncDiagnosticDeadline
	}
	if event.EndCallerState == SyncContextCanceled || event.StartCallerState == SyncContextCanceled || errors.Is(err, context.Canceled) {
		return SyncDiagnosticCanceled
	}
	// A private client timeout may wrap DeadlineExceeded while the observed
	// context remains active. That does not prove the original budget expired.
	// No error text or custom cancellation cause is read.
	if errors.Is(err, context.DeadlineExceeded) {
		if event.EndCallerState == SyncContextActive {
			return SyncDiagnosticTimeoutActive
		}
		return SyncDiagnosticDeadline
	}
	var timeout interface{ Timeout() bool }
	if errors.As(err, &timeout) && timeout.Timeout() && event.EndCallerState == SyncContextActive {
		return SyncDiagnosticTimeoutActive
	}
	return SyncDiagnosticError
}

func sanitizeSyncDiagnosticStage(stage SyncDiagnosticStage) SyncDiagnosticStage {
	switch stage {
	case SyncStageHookReplay, SyncStagePush, SyncStageRetention, SyncStagePreparation,
		SyncStageDocumentRead, SyncStageCanonical, SyncStageChunkPlan, SyncStageRequestMarshal,
		SyncStageRequestSetup, SyncStageTokenLookup, SyncStageHTTPDo, SyncStageHTTPBody,
		SyncStageHTTPGotConn, SyncStageHTTPWroteRequest, SyncStageHTTPFirstByte:
		return stage
	default:
		return SyncStageUnknown
	}
}

func sanitizeSyncDiagnosticRole(role SyncDiagnosticRole) SyncDiagnosticRole {
	switch role {
	case SyncRoleInventory, SyncRoleDocumentChunks:
		return role
	default:
		return SyncRoleOther
	}
}

func sanitizeSyncDiagnosticCounts(counts SyncDiagnosticCounts) SyncDiagnosticCounts {
	counts.Snapshots = max(0, counts.Snapshots)
	counts.Documents = max(0, counts.Documents)
	counts.Chunks = max(0, counts.Chunks)
	counts.Bytes = max(0, counts.Bytes)
	counts.HTTPStatus = max(0, counts.HTTPStatus)
	counts.ClientTimeoutMillis = max(0, counts.ClientTimeoutMillis)
	return counts
}

// These indices keep aggregate storage bounded even for unknown input labels.
func syncDiagnosticStageIndex(stage SyncDiagnosticStage) int {
	switch stage {
	case SyncStageHookReplay:
		return 0
	case SyncStagePush:
		return 1
	case SyncStageRetention:
		return 2
	case SyncStagePreparation:
		return 3
	case SyncStageDocumentRead:
		return 4
	case SyncStageCanonical:
		return 5
	case SyncStageChunkPlan:
		return 6
	case SyncStageRequestMarshal:
		return 7
	case SyncStageRequestSetup:
		return 8
	case SyncStageTokenLookup:
		return 9
	case SyncStageHTTPDo:
		return 10
	case SyncStageHTTPBody:
		return 11
	case SyncStageHTTPGotConn:
		return 12
	case SyncStageHTTPWroteRequest:
		return 13
	case SyncStageHTTPFirstByte:
		return 14
	default:
		return 15
	}
}

func syncDiagnosticAdd(a, b int64) int64 {
	const largest = int64(1<<63 - 1)
	if b > largest-a {
		return largest
	}
	return a + b
}
