package store

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
)

func TestDocumentCompressionSingleEncoderCompatibility(test *testing.T) {
	legacy, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault), zstd.WithEncoderConcurrency(4))
	if err != nil {
		test.Fatal(err)
	}
	defer legacy.Close()
	for _, body := range [][]byte{nil, []byte(`{"legacy":true}`), bytes.Repeat([]byte("typed content🙂\n"), 1<<15)} {
		before := bytes.Clone(body)
		for _, compressed := range [][]byte{docCompress(body), legacy.EncodeAll(body, nil)} {
			plain, err := docDecompress(compressed)
			if err != nil || !bytes.Equal(plain, body) || !bytes.Equal(before, body) {
				test.Fatal("compression changed content or compatibility", len(body), err)
			}
		}
	}
	raw := []byte(`{"uncompressed":"legacy"}`)
	if got, err := docDecompress(raw); err != nil || !bytes.Equal(got, raw) {
		test.Fatal("legacy uncompressed bytes changed", err)
	}
}

func TestDocumentCompressionConcurrentRoundTrips(test *testing.T) {
	const writers = 16
	start := make(chan struct{})
	results := make(chan error, writers)
	var workers sync.WaitGroup
	for ordinal := range writers {
		workers.Go(func() {
			<-start
			body := bytes.Repeat([]byte(fmt.Sprintf("writer-%d🙂\n", ordinal)), 8192)
			for range 8 {
				got, err := docDecompress(docCompress(body))
				if err != nil {
					results <- err
					return
				}
				if !bytes.Equal(got, body) {
					results <- fmt.Errorf("writer %d reused another caller's bytes", ordinal)
					return
				}
				if len(got) > 0 {
					got[0] ^= 0xff
				}
			}
			results <- nil
		})
	}
	close(start)
	workers.Wait()
	close(results)
	for err := range results {
		if err != nil {
			test.Fatal(err)
		}
	}
}

func TestDocumentCompressionRetainedEncoderBudget(test *testing.T) {
	mode := os.Getenv("CXT_TEST_COMPRESSION_MEMORY_CHILD")
	if mode == "" {
		for _, childMode := range []string{"bounded", "cpu-sized-control"} {
			test.Run(childMode, func(test *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestDocumentCompressionRetainedEncoderBudget$", "-test.count=1", "-test.v=true")
				command.Env = append(os.Environ(), "GOMAXPROCS=8", "GOMEMLIMIT=256MiB", "CXT_TEST_COMPRESSION_MEMORY_CHILD="+childMode)
				output, err := command.CombinedOutput()
				if err != nil {
					test.Fatalf("encoder retained-memory contract: mode=%s error=%v\n%s", childMode, err, output)
				}
				test.Logf("mode=%s\n%s", childMode, output)
			})
		}
		return
	}
	encoder := zstdEnc
	if mode == "cpu-sized-control" {
		var err error
		encoder, err = zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault), zstd.WithEncoderConcurrency(8))
		if err != nil {
			test.Fatal(err)
		}
		defer encoder.Close()
	} else if mode != "bounded" {
		test.Fatal("unknown compression memory mode", mode)
	}
	body := bytes.Repeat([]byte("bounded compression state🙂\n"), 1<<15)
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	var encodedBytes int
	for range 64 {
		encodedBytes += len(encoder.EncodeAll(body, nil))
	}
	runtime.GC()
	runtime.ReadMemStats(&after)
	runtime.KeepAlive(encoder)
	runtime.KeepAlive(body)
	retained := int64(after.HeapAlloc) - int64(before.HeapAlloc)
	const budget = 64 << 20
	if runtime.GOMAXPROCS(0) != 8 || encodedBytes == 0 {
		test.Fatal("isolated compression workload was not exercised", runtime.GOMAXPROCS(0), encodedBytes)
	}
	if mode == "bounded" && retained > budget || mode == "cpu-sized-control" && retained <= budget {
		test.Fatal("retained encoder memory did not distinguish bounded and CPU-sized pools", mode, retained, budget)
	}
	test.Logf("mode=%s gomaxprocs=8 retained_encoder_heap_bytes=%d budget_bytes=%d", mode, retained, budget)
}
