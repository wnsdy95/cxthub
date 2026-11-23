//go:build postgres

package app

import (
	"context"
	"errors"
	"reflect"
	"runtime"
	"strings"
	"sync/atomic"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type docRangeSourceHeaderPGInspection struct {
	handle         uintptr
	canonical      uintptr
	envelope       uintptr
	native         uintptr
	encoding       uintptr
	scalar         uintptr
	source         docRangeNativePagesPGSourceInspection
	phase          uint64
	nativePosition int64
	scalarPosition int64
	scalarEnd      int64
	scalarCopied   int64
	pending        int
	decodedCwd     int
	encodedCwd     int
	headerPhase    uint64
	headerBytes    int
	headerCopied   int64
	headerEnd      int64
	status         uint64
}

func docRangeSourceHeaderPGInspect(handle outbound.PreparedDocRangeWork) (docRangeSourceHeaderPGInspection, error) {
	capture := reflect.ValueOf(handle).Elem()
	state := docRangeSourceHeaderPGInspection{handle: reflect.ValueOf(handle).Pointer()}
	if !capture.FieldByName("normalization").IsNil() || capture.FieldByName("original").Bool() {
		return state, errors.New("canonical header candidate entered normalization or whole-document fallback")
	}
	canonical := capture.FieldByName("canonical")
	if canonical.IsNil() {
		return state, nil
	}
	preparation := canonical.Elem()
	envelope := preparation.FieldByName("envelope")
	if !envelope.IsValid() || !preparation.FieldByName("headerCopied").IsValid() {
		return state, errors.New("actual canonical PG preparation lacks the strict resumable header helper")
	}
	state.canonical = canonical.Pointer()
	state.headerPhase = preparation.FieldByName("phase").Uint()
	state.headerBytes = preparation.FieldByName("header").Len()
	state.headerCopied = preparation.FieldByName("headerCopied").Int()
	state.headerEnd = preparation.FieldByName("frame").FieldByName("envelopeEnd").Int()
	state.status = preparation.FieldByName("status").Uint()
	source := preparation.FieldByName("source")
	if source.FieldByName("Bytes").Int() == 0 && state.status == uint64(domain.LegacyByteSourceVerified) && envelope.IsNil() {
		return state, nil
	}
	var err error
	state.source, err = docRangeNativePagesPGSourceInspect(source, domain.ConversationManifestChunkBytes)
	if err != nil {
		return state, err
	}
	if envelope.IsNil() {
		return state, nil
	}
	if state.headerBytes != 0 || !capture.FieldByName("work").IsNil() || state.headerPhase != 1 {
		return state, errors.New("strict canonical header helper overlapped raw seed retention or published metadata work")
	}
	state.envelope = envelope.Pointer()
	helper := envelope.Elem()
	state.phase = helper.FieldByName("phase").Uint()
	state.decodedCwd = helper.FieldByName("decoded").FieldByName("Cwd").Len()
	native, encoding := helper.FieldByName("native"), helper.FieldByName("encoding")
	if !native.IsNil() && !encoding.IsNil() {
		return state, errors.New("canonical header retained native and encoder owners simultaneously")
	}
	if !native.IsNil() {
		state.native = native.Pointer()
		state.nativePosition = native.Elem().FieldByName("position").Int()
		scalar := native.Elem().FieldByName("scalar")
		if !scalar.IsNil() {
			state.scalar = scalar.Pointer()
			state.scalarPosition = scalar.Elem().FieldByName("position").Int()
			state.scalarEnd = scalar.Elem().FieldByName("end").Int()
			state.scalarCopied = scalar.Elem().FieldByName("copied").Int()
		}
	}
	if !encoding.IsNil() {
		state.encoding = encoding.Pointer()
		state.encodedCwd = encoding.Elem().FieldByName("envelope").FieldByName("Cwd").Len()
		state.pending = encoding.Elem().FieldByName("pending").Len()
		if state.decodedCwd != 0 || state.pending > domain.LegacyCanonicalEncodingBytes {
			return state, errors.New("canonical header encoder duplicated decoded ownership or exceeded its 64 KiB pending output")
		}
		scalar := encoding.Elem().FieldByName("scalar")
		if !scalar.IsNil() {
			frames := scalar.Elem().FieldByName("frames")
			if count := frames.FieldByName("count").Int(); count > 0 {
				frame, err := docRangeNativePagesPGFrame(frames, count-1)
				if err != nil {
					return state, err
				}
				if frame.FieldByName("kind").Uint() == 's' {
					state.scalar = scalar.Pointer()
					state.scalarPosition = frame.FieldByName("position").Int()
					state.scalarEnd = int64(frame.FieldByName("text").Len())
				}
			}
		}
	}
	return state, nil
}

type docRangeSourceHeaderPGContext struct {
	context.Context
	observe bool
	entered atomic.Bool
}

func (ctx *docRangeSourceHeaderPGContext) Err() error {
	if ctx.observe && !ctx.entered.Load() {
		var callers [20]uintptr
		frames := runtime.CallersFrames(callers[:runtime.Callers(2, callers[:])])
		for {
			frame, more := frames.Next()
			if strings.HasSuffix(frame.Function, "domain.(*legacySourceEnvelope).step") {
				ctx.entered.Store(true)
				break
			}
			if !more {
				break
			}
		}
	}
	return ctx.Context.Err()
}

type docRangeSourceHeaderPGStore struct {
	*docRangeWorkPGProbe
	work *docRangeSourceHeaderPGWork
}

type docRangeSourceHeaderPGWork struct {
	outbound.PreparedDocRangeWork
	handle          outbound.PreparedDocRangeWork
	inspection      *docRangeSourceHeaderPGContext
	before          docRangeSourceHeaderPGInspection
	after           docRangeSourceHeaderPGInspection
	beforeResources outbound.RootPublicationResources
	afterResources  outbound.RootPublicationResources
	failure         error
}

func (storage *docRangeSourceHeaderPGStore) PrepareDocRangeWork(ctx context.Context, job domain.DocRangeIndexJob) (outbound.PreparedDocRangeWork, error) {
	prepared, err := storage.docRangeWorkPGProbe.PrepareDocRangeWork(ctx, job)
	if prepared == nil || err != nil {
		return prepared, err
	}
	work, supported := prepared.(*docRangeWorkPGPrepared)
	if !supported {
		return prepared, errors.New("canonical header inspection lost the existing actual PG preparation probe")
	}
	storage.work = &docRangeSourceHeaderPGWork{PreparedDocRangeWork: prepared, handle: work.PreparedDocRangeWork}
	return storage.work, nil
}

func (work *docRangeSourceHeaderPGWork) Step(ctx context.Context, job domain.DocRangeIndexJob) (outbound.DocRangeWorkProgress, error) {
	var err error
	work.before, err = docRangeSourceHeaderPGInspect(work.handle)
	if err != nil {
		return 0, err
	}
	work.beforeResources, err = work.handle.RetainedResources()
	if err != nil {
		return 0, err
	}
	work.inspection = &docRangeSourceHeaderPGContext{Context: ctx, observe: work.before.envelope != 0}
	progress, stepErr := work.PreparedDocRangeWork.Step(work.inspection, job)
	work.after, err = docRangeSourceHeaderPGInspect(work.handle)
	if err != nil {
		work.failure = errors.Join(stepErr, err)
		return 0, work.failure
	}
	work.afterResources, err = work.handle.RetainedResources()
	work.failure = errors.Join(stepErr, err)
	return progress, work.failure
}
