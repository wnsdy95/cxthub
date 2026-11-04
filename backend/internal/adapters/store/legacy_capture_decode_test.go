package store

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strconv"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func legacyCaptureDecodeFixture(test *testing.T, stored []byte) (*legacyDocSpool, []legacyCapturePart) {
	test.Helper()
	source, err := newLegacyDocSpool()
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(func() {
		if err := source.Close(); err != nil {
			test.Error(err)
		}
	})
	var parts []legacyCapturePart
	for offset := 0; offset < len(stored); offset += domain.ConversationManifestChunkBytes {
		part := stored[offset:min(offset+domain.ConversationManifestChunkBytes, len(stored))]
		if err := source.Append(context.Background(), part); err != nil {
			test.Fatal(err)
		}
		parts = append(parts, legacyCapturePart{hash: domain.HashContent(part), bytes: len(part)})
	}
	return source, parts
}

func legacyCaptureDecodePrepare(test *testing.T, source *legacyDocSpool, parts []legacyCapturePart, extent int) *legacyCaptureDecodeWork {
	test.Helper()
	work, err := newLegacyCaptureDecodeWork(context.Background(), source, parts, extent)
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(func() {
		if err := work.Close(); err != nil {
			test.Error(err)
		}
	})
	return work
}

func legacyCaptureDecodeDrain(test *testing.T, work *legacyCaptureDecodeWork) error {
	test.Helper()
	for range 200 {
		before := work.decoded.size
		complete, err := work.Step(context.Background())
		if work.decoded.size-before > domain.ConversationManifestChunkBytes {
			test.Fatal("decode exceeded the output quantum", before, work.decoded.size)
		}
		if err != nil || complete {
			return err
		}
	}
	test.Fatal("decode did not reach a bounded terminal result")
	return nil
}

func TestLegacyCaptureDecodeQuantumAndStreamParity(test *testing.T) {
	for _, length := range []int{0, 1, domain.ConversationManifestChunkBytes, 3<<20 + 17} {
		for _, compressed := range []bool{false, true} {
			name := "plain"
			if compressed {
				name = "zstd"
			}
			test.Run(name+"/"+strconv.Itoa(length), func(test *testing.T) {
				body := bytes.Repeat([]byte("x"), length)
				stored := body
				if compressed {
					stored = docCompress(body)
				}
				source, parts := legacyCaptureDecodeFixture(test, stored)
				work := legacyCaptureDecodePrepare(test, source, parts, len(stored))
				if _, err := work.Bytes(context.Background()); !errors.Is(err, domain.ErrDocRangeIndexPending) {
					test.Fatal("unsealed decoded bytes escaped", err)
				}
				steps := 0
				for ; steps < 100; steps++ {
					before := work.decoded.size
					complete, err := work.Step(context.Background())
					if err != nil || work.decoded.size-before > domain.ConversationManifestChunkBytes {
						test.Fatal("invalid decode quantum", steps, before, work.decoded.size, err)
					}
					resources, err := work.RetainedResources()
					if err != nil || resources.SpoolBytes != int64(work.decoded.size) || resources.HeapBytes <= 0 || resources.HeapBytes > 2<<20 {
						test.Fatal("invalid known decoded resource accounting", resources, err)
					}
					if resources.KeepPermit != (docObjectIsCompressed(stored) && !complete) {
						test.Fatal("opaque decoder was parkable or remained pinned after release", resources, complete)
					}
					if complete {
						break
					}
				}
				if steps >= 100 || length > domain.ConversationManifestChunkBytes && steps == 0 {
					test.Fatal("decode did not advance in distinct bounded quanta", steps)
				}
				got, err := work.Bytes(context.Background())
				want, baselineErr := boundedLegacyObject(context.Background(), stored, maxLegacyReadBytes)
				if err != nil || baselineErr != nil || !bytes.Equal(got, body) || !bytes.Equal(got, want) {
					test.Fatal("streaming decode changed legacy bytes", len(got), len(want), err, baselineErr)
				}
				if len(got) > 0 {
					got[0] ^= 0xff
					owned, err := work.Bytes(context.Background())
					if err != nil || !bytes.Equal(owned, body) {
						test.Fatal("decoded readback borrowed caller mutation", err)
					}
				}
			})
		}
	}
}

func TestLegacyCaptureDecodeCompressionFailureAuthenticatesRemainingSource(test *testing.T) {
	stored := append([]byte{0x28, 0xb5, 0x2f, 0xfd, 0xff, 0xff, 0xff, 0xff}, bytes.Repeat([]byte("x"), 2<<20)...)
	for _, damage := range []bool{false, true} {
		name := "invalid-frame"
		if damage {
			name = "invalid-frame-and-private-damage"
		}
		test.Run(name, func(test *testing.T) {
			source, parts := legacyCaptureDecodeFixture(test, stored)
			work := legacyCaptureDecodePrepare(test, source, parts, len(stored))
			if complete, err := work.Step(context.Background()); complete || err != nil {
				test.Fatal("semantic error escaped before remaining private input authentication", complete, err)
			}
			if damage {
				if _, err := source.file.WriteAt([]byte("!"), int64(len(stored)-1)); err != nil {
					test.Fatal(err)
				}
			}
			err := legacyCaptureDecodeDrain(test, work)
			if damage && !errors.Is(err, domain.ErrPreparedDocRangeObject) || !damage && (!errors.Is(err, domain.ErrIntegrity) || errors.Is(err, domain.ErrPreparedDocRangeObject)) {
				test.Fatal("decode/private failure priority changed", damage, err)
			}
		})
	}
}

func TestLegacyCaptureDecodeLimitsAndConcatenatedFrames(test *testing.T) {
	joined := append(docCompress([]byte("first")), docCompress([]byte("second"))...)
	for _, fixture := range []struct {
		name    string
		stored  []byte
		want    []byte
		wantErr error
	}{
		{name: "concatenated", stored: joined, want: []byte("firstsecond")},
		{name: "truncated", stored: []byte{0x28, 0xb5, 0x2f, 0xfd}, wantErr: domain.ErrIntegrity},
		{name: "trailing", stored: append(docCompress([]byte("first")), []byte("junk")...), wantErr: domain.ErrIntegrity},
		{name: "decoded-limit", stored: docCompress(bytes.Repeat([]byte("x"), maxLegacyReadBytes+1)), wantErr: domain.ErrDocumentResourceLimit},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			source, parts := legacyCaptureDecodeFixture(test, fixture.stored)
			work := legacyCaptureDecodePrepare(test, source, parts, len(fixture.stored))
			err := legacyCaptureDecodeDrain(test, work)
			baseline, baselineErr := boundedLegacyObject(context.Background(), fixture.stored, maxLegacyReadBytes)
			if fixture.wantErr != nil {
				if !errors.Is(err, fixture.wantErr) || !errors.Is(baselineErr, fixture.wantErr) {
					test.Fatal("legacy decode error changed", err, baselineErr, fixture.wantErr)
				}
				return
			}
			got, readErr := work.Bytes(context.Background())
			if err != nil || readErr != nil || baselineErr != nil || !bytes.Equal(got, fixture.want) || !bytes.Equal(got, baseline) {
				test.Fatal("legacy concatenated frame bytes changed", err, readErr, baselineErr)
			}
		})
	}
}

func TestLegacyCaptureDecodePrivateReadbackCancellationAndClose(test *testing.T) {
	for _, damage := range []string{"checksum", "truncate", "extra", "cancel", "close"} {
		test.Run(damage, func(test *testing.T) {
			source, parts := legacyCaptureDecodeFixture(test, bytes.Repeat([]byte("x"), 1<<20+1))
			work := legacyCaptureDecodePrepare(test, source, parts, source.size)
			if err := legacyCaptureDecodeDrain(test, work); err != nil {
				test.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var mutation error
			switch damage {
			case "checksum":
				_, mutation = work.decoded.file.WriteAt([]byte("!"), int64(work.decoded.size-1))
			case "truncate":
				mutation = work.decoded.file.Truncate(int64(work.decoded.size - 1))
			case "extra":
				_, mutation = work.decoded.file.WriteAt([]byte("!"), int64(work.decoded.size))
			case "cancel":
				cancel()
			case "close":
				mutation = work.Close()
			}
			if mutation != nil {
				test.Fatal(mutation)
			}
			_, err := work.Bytes(ctx)
			want := domain.ErrPreparedDocRangeObject
			if damage == "cancel" {
				want = context.Canceled
			}
			if damage == "close" {
				want = os.ErrClosed
			}
			if !errors.Is(err, want) {
				test.Fatal("decoded private error changed", damage, err)
			}
		})
	}
}
