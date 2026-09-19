package app

import (
	"context"
	"errors"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
	"strings"
	"sync"
	"testing"
)

type noticeSelectionFake struct {
	value  domain.SessionNoticeSelection
	reads  int
	change func(int, *domain.SessionNoticeSelection)
	err    error
}

func (f *noticeSelectionFake) ReadNoticeSelection(context.Context, string) (domain.SessionNoticeSelection, error) {
	f.reads++
	v := f.value
	if f.change != nil {
		f.change(f.reads, &v)
	}
	return v, f.err
}

type noticeCursorFake struct {
	mu     sync.Mutex
	last   map[domain.SessionNoticeScope]domain.ContentHash
	ackErr error
}

func (f *noticeCursorFake) WithSessionNoticeCursor(_ context.Context, s domain.SessionNoticeScope, fn func(domain.ContentHash) (domain.ContentHash, error)) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.last == nil {
		f.last = map[domain.SessionNoticeScope]domain.ContentHash{}
	}
	next, err := fn(f.last[s])
	if err != nil {
		return err
	}
	if f.ackErr != nil {
		return f.ackErr
	}
	f.last[s] = next
	return nil
}
func noticeFixture() (*noticeSelectionFake, *noticeCursorFake, inbound.SessionNoticeInput) {
	return &noticeSelectionFake{value: domain.SessionNoticeSelection{RepoID: string(domain.HashContent([]byte("repo"))), WorktreeID: strings.Repeat("a", 32), BranchID: "branch", Snapshot: domain.HashContent([]byte("selected")), CodeCommit: strings.Repeat("b", 40), MemoryPinned: true}}, &noticeCursorFake{}, inbound.SessionNoticeInput{Cwd: "/work", Provider: domain.ProviderCodex, SessionID: "session-a"}
}
func TestSessionNoticeQuietAndSelectionChanges(t *testing.T) {
	src, cursors, in := noticeFixture()
	svc := NewSessionNoticeService(src, cursors)
	var got []domain.SessionNotice
	emit := func(n domain.SessionNotice) error { got = append(got, n); return nil }
	for i := 0; i < 3; i++ {
		if err := svc.DeliverSessionNotice(context.Background(), in, emit); err != nil {
			t.Fatal(err)
		}
	}
	if len(got) != 1 {
		t.Fatalf("unchanged output=%d", len(got))
	}
	original := got[0].ID
	src.value.CodeCommit = strings.Repeat("c", 40)
	if err := svc.DeliverSessionNotice(context.Background(), in, emit); err != nil {
		t.Fatal(err)
	}
	src.value.MemoryHash = domain.HashContent([]byte("repin"))
	src.value.MemorySource = src.value.Snapshot
	if err := svc.DeliverSessionNotice(context.Background(), in, emit); err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[1].ID == original || got[2].ID == got[1].ID {
		t.Fatalf("changes=%+v", got)
	}
	for _, n := range got {
		if len(n.Text()) > 2048 {
			t.Fatal("unbounded notice")
		}
	}
	if !strings.Contains(got[0].Text(), "Keep it empty") || !strings.Contains(got[2].Text(), "mode=effective") {
		t.Fatal("lost historical memory semantics")
	}
}
func TestSessionNoticeDeliveryFailuresRemainRetryable(t *testing.T) {
	for _, ackFailure := range []bool{false, true} {
		t.Run(map[bool]string{false: "output", true: "ack"}[ackFailure], func(t *testing.T) {
			src, cursors, in := noticeFixture()
			svc := NewSessionNoticeService(src, cursors)
			failure := errors.New("write failed")
			var ids []domain.ContentHash
			if ackFailure {
				cursors.ackErr = failure
			}
			emit := func(n domain.SessionNotice) error {
				ids = append(ids, n.ID)
				if !ackFailure && len(ids) == 1 {
					return failure
				}
				return nil
			}
			if err := svc.DeliverSessionNotice(context.Background(), in, emit); !errors.Is(err, failure) {
				t.Fatalf("failure=%v", err)
			}
			if len(cursors.last) != 0 {
				t.Fatal("failed delivery acknowledged")
			}
			cursors.ackErr = nil
			if err := svc.DeliverSessionNotice(context.Background(), in, emit); err != nil {
				t.Fatal(err)
			}
			if err := svc.DeliverSessionNotice(context.Background(), in, emit); err != nil {
				t.Fatal(err)
			}
			if len(ids) != 2 || ids[0] != ids[1] {
				t.Fatalf("retry IDs=%v", ids)
			}
		})
	}
}
func TestSessionNoticeRejectsChangesDuringLockAcquisition(t *testing.T) {
	src, cursors, in := noticeFixture()
	src.change = func(read int, v *domain.SessionNoticeSelection) {
		if read == 2 {
			v.MemoryHash = domain.HashContent([]byte("raced"))
			v.MemorySource = v.Snapshot
		}
	}
	emitted := false
	err := NewSessionNoticeService(src, cursors).DeliverSessionNotice(context.Background(), in, func(domain.SessionNotice) error { emitted = true; return nil })
	if !errors.Is(err, domain.ErrSelectionChanged) || emitted || len(cursors.last) != 0 {
		t.Fatalf("stale delivery: %v %v", err, emitted)
	}
}
func TestSessionNoticeScopeIsolation(t *testing.T) {
	src, cursors, in := noticeFixture()
	svc := NewSessionNoticeService(src, cursors)
	count := 0
	emit := func(domain.SessionNotice) error { count++; return nil }
	deliver := func() {
		t.Helper()
		if err := svc.DeliverSessionNotice(context.Background(), in, emit); err != nil {
			t.Fatal(err)
		}
	}
	deliver()
	in.SessionID = "session-b"
	deliver()
	in.Provider = domain.ProviderClaude
	deliver()
	src.value.WorktreeID = strings.Repeat("c", 32)
	deliver()
	src.value.RepoID = string(domain.HashContent([]byte("repo2")))
	deliver()
	deliver()
	if count != 5 {
		t.Fatalf("scope emissions=%d", count)
	}
}
func TestSessionNoticeCanceledInvalidAndUninitialized(t *testing.T) {
	for _, kind := range []string{"cancel", "invalid", "uninitialized"} {
		t.Run(kind, func(t *testing.T) {
			src, cursors, in := noticeFixture()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if kind == "cancel" {
				cancel()
			}
			if kind == "invalid" {
				src.value.RepoID = "foreign"
			}
			if kind == "uninitialized" {
				src.err = domain.ErrNotFound
			}
			err := NewSessionNoticeService(src, cursors).DeliverSessionNotice(ctx, in, func(domain.SessionNotice) error { t.Fatal("unexpected output"); return nil })
			if kind == "uninitialized" && err != nil {
				t.Fatal(err)
			}
			if kind != "uninitialized" && err == nil {
				t.Fatal("missing failure")
			}
			if len(cursors.last) > 0 {
				t.Fatal("acknowledged")
			}
		})
	}
}
