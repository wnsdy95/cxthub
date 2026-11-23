package graphdiagnostics

import (
	"context"
	"log/slog"
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

type Recorder struct {
	mutex  sync.Mutex
	stages [stageCount]StageStats
	sql    SQLStats
}

type contextKey struct{}

type scope struct {
	recorder *Recorder
	stage    Stage
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

func milliseconds(duration time.Duration) float64 {
	return float64(duration) / float64(time.Millisecond)
}
