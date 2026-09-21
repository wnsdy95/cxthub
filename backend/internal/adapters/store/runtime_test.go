package store

import (
	"context"
	"errors"
	"fmt"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func checkRuntime(t *testing.T, st interface {
	outbound.RuntimeStore
	outbound.RepositoryStore
}) {
	ctx := context.Background()
	now := time.Now().UTC()
	user := domain.User{ID: "runtime-user", Username: "runtime-user", Email: "runtime@example.test", Name: "Runtime"}
	if err := st.UpsertUser(ctx, user); err != nil {
		t.Fatal(err)
	}
	p := domain.DevicePairing{Code: "BCF-234", PollHash: domain.HashToken("private poll secret"), ExpiresAt: now.Add(time.Minute)}
	if err := st.CreateDevicePairing(ctx, p); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateDevicePairing(ctx, p); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("collision=%v", err)
	}
	if _, err := st.GetDevicePairing(ctx, p.Code, domain.HashToken("wrong secret"), now); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("wrong secret=%v", err)
	}
	if err := st.ApproveDevicePairing(ctx, p.Code, user.ID, now); err != nil {
		t.Fatal(err)
	}
	if err := st.ApproveDevicePairing(ctx, p.Code, "another-user", now); err == nil {
		t.Fatal("second approver replaced owner")
	}
	var wg sync.WaitGroup
	var accepted atomic.Int32
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sess := domain.Session{Token: domain.HashToken(fmt.Sprint("session-", i)), UserID: user.ID, Kind: "cli", CreatedAt: now, ExpiresAt: now.Add(time.Hour)}
			err := st.RedeemDevicePairing(ctx, p.Code, p.PollHash, sess, now)
			if err == nil {
				accepted.Add(1)
			} else if !errors.Is(err, domain.ErrNotFound) {
				t.Errorf("redeem=%v", err)
			}
		}(i)
	}
	wg.Wait()
	if accepted.Load() != 1 {
		t.Fatalf("tokens issued=%d", accepted.Load())
	}
	if _, err := st.GetDevicePairing(ctx, p.Code, p.PollHash, now); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("replayed pairing=%v", err)
	}
	key := "runtime-test:" + domain.NewID("")
	accepted.Store(0)
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			yes, err := st.AllowRequest(ctx, key, 7, time.Minute, now)
			if err != nil {
				t.Error(err)
			}
			if yes {
				accepted.Add(1)
			}
		}()
	}
	wg.Wait()
	if accepted.Load() != 7 {
		t.Fatalf("aggregate allowance=%d", accepted.Load())
	}
	if yes, err := st.AllowRequest(ctx, key, 7, time.Minute, now.Add(time.Minute)); err != nil || !yes {
		t.Fatalf("refill=%v %v", yes, err)
	}
	if err := st.PruneRuntimeState(ctx, now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
}
func TestFSSharedRuntime(t *testing.T) { checkRuntime(t, NewFSStore(t.TempDir())) }
