//go:build darwin || linux

package store

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestStoredConversationReferencePerformance(test *testing.T) {
	source := os.Getenv("CXT_TEST_ROOT_REFERENCE_SOURCE")
	if source == "" {
		test.Skip("opt-in private local root reference measurement")
	}
	hash := domain.ContentHash(os.Getenv("CXT_TEST_ROOT_REFERENCE_TARGET"))
	if domain.ValidateContentHash(hash) != nil {
		test.Fatal("invalid private reference fixture target")
	}
	if mode := os.Getenv("CXT_TEST_ROOT_REFERENCE_WORKER"); mode != "" {
		measureStoredConversationReference(test, source, hash, mode)
		return
	}
	executable, err := os.Executable()
	if err != nil {
		test.Fatal("cannot locate reference measurement executable")
	}
	for _, mode := range []string{"single-pass", "two-pass", "warm-two-pass"} {
		arguments := []string{executable, "-test.run=^TestStoredConversationReferencePerformance$", "-test.v", "-test.timeout=10m"}
		switch runtime.GOOS {
		case "darwin":
			arguments = append([]string{"/usr/bin/time", "-l"}, arguments...)
		case "linux":
			arguments = append([]string{"/usr/bin/time", "-v"}, arguments...)
		default:
			test.Skip("reference RSS measurement requires Darwin or Linux time")
		}
		command := exec.Command(arguments[0], arguments[1:]...)
		for _, variable := range os.Environ() {
			if !strings.HasPrefix(variable, "GOMEMLIMIT=") && !strings.HasPrefix(variable, "GOMAXPROCS=") && !strings.HasPrefix(variable, "CXT_TEST_ROOT_REFERENCE_WORKER=") {
				command.Env = append(command.Env, variable)
			}
		}
		command.Env = append(command.Env, "GOMEMLIMIT=384MiB", "GOMAXPROCS=1", "CXT_TEST_ROOT_REFERENCE_WORKER="+mode)
		output, err := command.CombinedOutput()
		test.Logf("mode=%s worker:\n%s", mode, output)
		if err != nil {
			test.Fatal("private reference measurement worker failed")
		}
	}
}

func measureStoredConversationReference(test *testing.T, source string, hash domain.ContentHash, mode string) {
	test.Helper()
	passes := 1
	switch mode {
	case "single-pass":
	case "two-pass":
		passes = 2
	case "warm-two-pass":
		passes = 3
	default:
		test.Fatal("unknown reference measurement mode")
	}
	if debug.SetMemoryLimit(-1) != 384<<20 || runtime.GOMAXPROCS(0) != 1 {
		test.Fatal("reference measurement requires GOMEMLIMIT=384MiB and GOMAXPROCS=1")
	}
	runtime.GC()
	var baseline runtime.MemStats
	runtime.ReadMemStats(&baseline)
	started := time.Now()
	var verifier domain.CanonicalDocVerifier
	var measuredWall time.Duration
	var measuredUser, measuredSystem time.Duration
	for pass := 1; pass <= passes; pass++ {
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		userBefore, systemBefore := referencePerformanceCPU(test)
		passStarted := time.Now()
		ctx := context.Background()
		raw, err := os.ReadFile(filepath.Join(source, "docs", strings.TrimPrefix(string(hash), "sha256:")))
		if err != nil {
			test.Fatal("cannot read private root fixture")
		}
		manifest, root, err := storedConversationManifest(ctx, raw)
		if err != nil || !root {
			test.Fatal("private reference fixture is not a valid root")
		}
		reads, storedBytes := 0, int64(0)
		proof, err := verifyStoredConversationReference(ctx, &verifier, hash, manifest, func(ctx context.Context, chunk domain.ContentHash) ([]byte, error) {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			body, err := os.ReadFile(filepath.Join(source, "chunks", strings.TrimPrefix(string(chunk), "sha256:")))
			reads++
			storedBytes += int64(len(body))
			return body, err
		})
		elapsed := time.Since(passStarted)
		userAfter, systemAfter := referencePerformanceCPU(test)
		user, system := userAfter-userBefore, systemAfter-systemBefore
		if mode != "warm-two-pass" || pass > 1 {
			measuredWall += elapsed
			measuredUser += user
			measuredSystem += system
		}
		if err != nil || proof.DocumentRef() != (domain.DocumentRef{Hash: hash, Identity: domain.DocumentIdentityRootV1}) || reads != len(manifest.Chunks) {
			test.Fatal("private root reference verification failed")
		}
		runtime.ReadMemStats(&after)
		test.Logf("mode=%s pass=%d stream=%d events=%d chunks=%d chunk_reads=%d stored_bytes=%d wall_seconds=%.6f user_seconds=%.6f system_seconds=%.6f allocated_bytes=%d heap_after_bytes=%d gc_cycles=%d", mode, pass, manifest.StreamBytes, manifest.EventCount, len(manifest.Chunks), reads, storedBytes, elapsed.Seconds(), user.Seconds(), system.Seconds(), after.TotalAlloc-before.TotalAlloc, after.HeapAlloc, after.NumGC-before.NumGC)
	}
	elapsed := time.Since(started)
	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	runtime.KeepAlive(&verifier)
	test.Logf("mode=%s passes=%d total_wall_seconds=%.6f measured_wall_seconds=%.6f measured_user_seconds=%.6f measured_system_seconds=%.6f baseline_heap_bytes=%d final_live_heap_bytes=%d gomemlimit_bytes=%d gomaxprocs=%d", mode, passes, elapsed.Seconds(), measuredWall.Seconds(), measuredUser.Seconds(), measuredSystem.Seconds(), baseline.HeapAlloc, after.HeapAlloc, debug.SetMemoryLimit(-1), runtime.GOMAXPROCS(0))
}

func referencePerformanceCPU(test *testing.T) (time.Duration, time.Duration) {
	test.Helper()
	var usage syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &usage); err != nil {
		test.Fatal("reference CPU usage unavailable")
	}
	return time.Duration(usage.Utime.Sec)*time.Second + time.Duration(usage.Utime.Usec)*time.Microsecond,
		time.Duration(usage.Stime.Sec)*time.Second + time.Duration(usage.Stime.Usec)*time.Microsecond
}
