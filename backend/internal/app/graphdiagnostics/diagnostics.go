package graphdiagnostics

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
)

type Stage uint8

const (
	Query Stage = iota
	LegacyRead
	LoadView
	Metadata
	Refs
	Snapshots
	Reflog
	History
	Pending
	Unsync
	Archives
	Revision
	ViewProjection
	CachedProjection
	SourceGeneration
	Fingerprint
	BuildAdmission
	CacheLookup
	CacheDecode
	CacheEncode
	Projection
	GraphBase
	PREvidence
	GitEvidence
	BranchProjection
	PublishedRead
	PublishedLoad
	PublishedDecode
	PublishedQualification
	PositionProjection
	Repair
	stageCount
)

var stageNames = [stageCount]string{
	"query", "legacy_read", "load_view", "metadata", "refs", "snapshots", "reflog", "history", "pending", "unsync",
	"archives", "revision", "view_projection", "cached_projection", "source_generation", "fingerprint",
	"build_admission", "cache_lookup", "cache_decode", "cache_encode", "projection", "graph_base", "pr_evidence",
	"git_evidence", "branch_projection", "published_read", "published_load", "published_decode", "published_qualification",
	"position_projection", "repair",
}

type Route uint8

const (
	Legacy Route = iota
	Published
)

type Cache uint8

const (
	GraphCache Cache = iota
	BranchCache
)

type CacheResult uint8

const (
	Hit CacheResult = iota
	Miss
	Ineligible
)

type SQLStats struct {
	Count       uint64  `json:"count"`
	ElapsedMS   float64 `json:"elapsed_ms"`
	Rows        int64   `json:"rows"`
	RowsKnown   uint64  `json:"rows_known"`
	RowsUnknown uint64  `json:"rows_unknown"`
	Errors      uint64  `json:"errors"`
}

type StageStats struct {
	Name      string   `json:"name"`
	Count     uint64   `json:"count"`
	ElapsedMS float64  `json:"elapsed_ms"`
	SQL       SQLStats `json:"sql"`
}

func (stats SQLStats) LogValue() slog.Value {
	return slog.GroupValue(slog.Uint64("count", stats.Count), slog.Float64("elapsed_ms", stats.ElapsedMS),
		slog.Int64("rows", stats.Rows), slog.Uint64("rows_known", stats.RowsKnown),
		slog.Uint64("rows_unknown", stats.RowsUnknown), slog.Uint64("errors", stats.Errors))
}

type CacheStats struct {
	Name       string `json:"name"`
	Hit        uint64 `json:"hit"`
	Miss       uint64 `json:"miss"`
	Ineligible uint64 `json:"ineligible"`
}

type Snapshot struct {
	Route          string       `json:"route"`
	LegacyReads    uint64       `json:"legacy_reads"`
	PublishedReads uint64       `json:"published_reads"`
	SQL            SQLStats     `json:"sql"`
	Stages         []StageStats `json:"stages"`
	Caches         []CacheStats `json:"caches"`
}

type Recorder struct {
	mutex  sync.Mutex
	stages [stageCount]StageStats
	sql    SQLStats
	routes [2]uint64
	caches [2][3]uint64
}

func (snapshot Snapshot) LogValue() slog.Value {
	stages := make([]slog.Attr, 0, len(snapshot.Stages))
	for _, stage := range snapshot.Stages {
		stages = append(stages, slog.Any(stage.Name, slog.GroupValue(slog.Uint64("count", stage.Count),
			slog.Float64("elapsed_ms", stage.ElapsedMS), slog.Any("sql", stage.SQL))))
	}
	caches := make([]slog.Attr, 0, len(snapshot.Caches))
	for _, cache := range snapshot.Caches {
		caches = append(caches, slog.Any(cache.Name, slog.GroupValue(slog.Uint64("hit", cache.Hit),
			slog.Uint64("miss", cache.Miss), slog.Uint64("ineligible", cache.Ineligible))))
	}
	return slog.GroupValue(slog.String("route", snapshot.Route), slog.Uint64("legacy_reads", snapshot.LegacyReads),
		slog.Uint64("published_reads", snapshot.PublishedReads), slog.Any("sql", snapshot.SQL),
		slog.Any("stages", slog.GroupValue(stages...)), slog.Any("caches", slog.GroupValue(caches...)))
}

type contextKey struct{}

type scope struct {
	recorder *Recorder
	stage    Stage
}

func NewContext(ctx context.Context) (context.Context, *Recorder) {
	recorder := &Recorder{}
	return context.WithValue(ctx, contextKey{}, scope{recorder: recorder, stage: stageCount}), recorder
}

type Span struct {
	scope
	started time.Time
}

func Start(ctx context.Context, stage Stage) (context.Context, *Span) {
	parent, _ := ctx.Value(contextKey{}).(scope)
	if parent.recorder == nil || stage >= stageCount {
		return ctx, nil
	}
	child := scope{recorder: parent.recorder, stage: stage}
	return context.WithValue(ctx, contextKey{}, child), &Span{scope: child, started: time.Now()}
}

func (span *Span) End() {
	if span == nil {
		return
	}
	elapsed := time.Since(span.started)
	span.recorder.mutex.Lock()
	stats := &span.recorder.stages[span.stage]
	stats.Count++
	stats.ElapsedMS += milliseconds(elapsed)
	span.recorder.mutex.Unlock()
}

func Read[Value any](ctx context.Context, stage Stage, read func(context.Context) (Value, error)) (Value, error) {
	bound, span := Start(ctx, stage)
	defer span.End()
	return read(bound)
}

func SelectRoute(ctx context.Context, route Route) {
	current, _ := ctx.Value(contextKey{}).(scope)
	if current.recorder == nil || route > Published {
		return
	}
	current.recorder.mutex.Lock()
	current.recorder.routes[route]++
	current.recorder.mutex.Unlock()
}

func RecordCache(ctx context.Context, cache Cache, result CacheResult) {
	current, _ := ctx.Value(contextKey{}).(scope)
	if current.recorder == nil || cache > BranchCache || result > Ineligible {
		return
	}
	current.recorder.mutex.Lock()
	current.recorder.caches[cache][result]++
	current.recorder.mutex.Unlock()
}

type SQLQuery struct {
	scope
	started time.Time
}

func StartSQL(ctx context.Context) *SQLQuery {
	current, _ := ctx.Value(contextKey{}).(scope)
	if current.recorder == nil {
		return nil
	}
	return &SQLQuery{scope: current, started: time.Now()}
}

func (query *SQLQuery) End(rows int64, known, failed bool) {
	if query == nil {
		return
	}
	elapsed := milliseconds(time.Since(query.started))
	query.recorder.mutex.Lock()
	defer query.recorder.mutex.Unlock()
	addSQL(&query.recorder.sql, elapsed, rows, known, failed)
	if query.stage < stageCount {
		addSQL(&query.recorder.stages[query.stage].SQL, elapsed, rows, known, failed)
	}
}

func addSQL(stats *SQLStats, elapsed float64, rows int64, known, failed bool) {
	stats.Count++
	stats.ElapsedMS += elapsed
	if known && rows >= 0 && !failed {
		stats.Rows += rows
		stats.RowsKnown++
	} else {
		stats.RowsUnknown++
	}
	if failed {
		stats.Errors++
	}
}

func (recorder *Recorder) Snapshot() Snapshot {
	recorder.mutex.Lock()
	defer recorder.mutex.Unlock()
	result := Snapshot{Route: "unselected", SQL: recorder.sql, LegacyReads: recorder.routes[Legacy], PublishedReads: recorder.routes[Published], Stages: []StageStats{}, Caches: []CacheStats{}}
	switch {
	case result.LegacyReads > 0 && result.PublishedReads > 0:
		result.Route = "mixed"
	case result.LegacyReads > 0:
		result.Route = "legacy"
	case result.PublishedReads > 0:
		result.Route = "published"
	}
	for stage, stats := range recorder.stages {
		if stats.Count == 0 && stats.SQL.Count == 0 {
			continue
		}
		stats.Name = stageNames[stage]
		result.Stages = append(result.Stages, stats)
	}
	for cache, name := range []string{"graph", "branch"} {
		counts := recorder.caches[cache]
		if counts != [3]uint64{} {
			result.Caches = append(result.Caches, CacheStats{Name: name, Hit: counts[Hit], Miss: counts[Miss], Ineligible: counts[Ineligible]})
		}
	}
	return result
}

func (snapshot Snapshot) ServerTiming(elapsed time.Duration) string {
	var header strings.Builder
	fmt.Fprintf(&header, "graph_pre;dur=%.3f, graph_sql;dur=%.3f", milliseconds(elapsed), snapshot.SQL.ElapsedMS)
	for _, stage := range snapshot.Stages {
		if stage.Count > 0 {
			fmt.Fprintf(&header, ", %s;dur=%.3f", stage.Name, stage.ElapsedMS)
		}
	}
	return header.String()
}

func milliseconds(duration time.Duration) float64 {
	return float64(duration) / float64(time.Millisecond)
}
