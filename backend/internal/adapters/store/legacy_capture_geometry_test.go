package store

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"os"
	"strconv"
	"testing"
	"unsafe"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type legacyCaptureGeometryFactory struct {
	name      string
	partBytes int
	source    func(context.Context) (domain.LegacyByteSource, error)
}

func legacyCaptureGeometryFactories(work *legacyCaptureDecodeWork) [2]legacyCaptureGeometryFactory {
	return [2]legacyCaptureGeometryFactory{
		{name: "proof", partBytes: domain.ConversationManifestChunkBytes, source: work.proofSource},
		{name: "normalization", partBytes: domain.LegacyNormalizationPartBytes, source: work.normalizationSource},
	}
}

func legacyCaptureGeometryStored(test *testing.T, representation string, body []byte) ([]byte, []byte) {
	test.Helper()
	switch representation {
	case "plain":
		return body, body
	case "zstd":
		return docCompress(body), body
	case "gzip-opaque":
		var buffer bytes.Buffer
		writer, err := gzip.NewWriterLevel(&buffer, gzip.NoCompression)
		if err != nil {
			test.Fatal(err)
		}
		if _, err := writer.Write(body); err != nil {
			test.Fatal(err)
		}
		if err := writer.Close(); err != nil {
			test.Fatal(err)
		}
		return buffer.Bytes(), buffer.Bytes()
	default:
		test.Fatal("unknown capture representation", representation)
		return nil, nil
	}
}

func checkLegacyCaptureGeometrySource(test *testing.T, factory legacyCaptureGeometryFactory, source domain.LegacyByteSource, want []byte) {
	test.Helper()
	ctx := context.Background()
	if source.Bytes != len(want) || source.Read == nil || len(source.Parts) != (len(want)+factory.partBytes-1)/factory.partBytes {
		test.Fatal("capture source changed extent or manifest geometry", factory.name, source.Bytes, len(source.Parts), len(want))
	}
	if factory.name == "proof" && source.PartBytes != 0 && source.PartBytes != domain.ConversationManifestChunkBytes ||
		factory.name == "normalization" && source.PartBytes != domain.LegacyNormalizationPartBytes {
		test.Fatal("capture selected the wrong authenticated part size", factory.name, source.PartBytes)
	}
	var joined []byte
	for ordinal, part := range source.Parts {
		offset := ordinal * factory.partBytes
		length := min(factory.partBytes, len(want)-offset)
		if part.Bytes != int64(length) || part.Hash != domain.HashContent(want[offset:offset+length]) {
			test.Fatal("capture manifest does not describe actual decoded bytes", factory.name, ordinal, part)
		}
		body, err := source.Read(ctx, offset, length)
		if err != nil || len(body) != length || domain.HashContent(body) != part.Hash || !bytes.Equal(body, want[offset:offset+length]) {
			test.Fatal("capture source read disagrees with its authenticated descriptor", factory.name, ordinal, len(body), err)
		}
		joined = append(joined, body...)
		body[0] ^= 0xff
		owned, err := source.Read(ctx, offset, length)
		if err != nil || !bytes.Equal(owned, want[offset:offset+length]) {
			test.Fatal("capture source read borrowed caller-mutated bytes", factory.name, ordinal, err)
		}
	}
	if !bytes.Equal(joined, want) {
		test.Fatal("source geometry changed reconstructed original bytes", factory.name)
	}
	if body, err := source.Read(ctx, len(want), 0); err != nil || len(body) != 0 {
		test.Fatal("capture source lost its exact EOF", factory.name, len(body), err)
	}
	if body, err := source.Read(ctx, len(want), 1); err == nil || len(body) != 0 {
		test.Fatal("capture source admitted bytes beyond EOF", factory.name, len(body), err)
	}
}

func TestLegacyCaptureGeometrySourceParityAndDescriptorOwnership(test *testing.T) {
	for _, representation := range []string{"plain", "zstd", "gzip-opaque"} {
		for _, length := range []int{0, 1, 64<<10 - 1, 64 << 10, 64<<10 + 1, 512<<10 - 1, 512 << 10, 512<<10 + 1, 3<<20 + 17} {
			test.Run(representation+"/"+strconv.Itoa(length), func(test *testing.T) {
				body := make([]byte, length)
				for offset := range body {
					body[offset] = byte((offset*31 + offset/(64<<10)) % 251)
				}
				stored, want := legacyCaptureGeometryStored(test, representation, body)
				original, parts := legacyCaptureDecodeFixture(test, stored)
				work := legacyCaptureDecodePrepare(test, original, parts, len(stored))
				if err := legacyCaptureDecodeDrain(test, work); err != nil {
					test.Fatal(err)
				}
				decoded, err := work.Bytes(context.Background())
				baseline, baselineErr := boundedLegacyObject(context.Background(), stored, maxLegacyReadBytes)
				if err != nil || baselineErr != nil || !bytes.Equal(decoded, want) || !bytes.Equal(decoded, baseline) {
					test.Fatal("dual capture manifests changed legacy representation semantics", representation, err, baselineErr)
				}
				for _, factory := range legacyCaptureGeometryFactories(work) {
					source, err := factory.source(context.Background())
					if err != nil {
						test.Fatal(factory.name, err)
					}
					checkLegacyCaptureGeometrySource(test, factory, source, want)
					if len(source.Parts) == 0 {
						continue
					}
					prior, err := factory.source(context.Background())
					if err != nil {
						test.Fatal(err)
					}
					first := source.Parts[0]
					source.Parts[0].Bytes++
					source.Parts[0].Hash = domain.HashContent([]byte("caller manifest mutation"))
					fresh, err := factory.source(context.Background())
					if err != nil || len(fresh.Parts) != len(prior.Parts) || fresh.Parts[0] != first || prior.Parts[0] != first {
						test.Fatal("capture manifest aliases a caller descriptor slice", factory.name, err)
					}
				}
				for _, factory := range legacyCaptureGeometryFactories(work) {
					source, err := factory.source(context.Background())
					if err != nil {
						test.Fatal(err)
					}
					checkLegacyCaptureGeometrySource(test, factory, source, want)
				}
			})
		}
	}
}

func TestLegacyCaptureGeometryUnfinishedCancellationAndClose(test *testing.T) {
	for _, representation := range []string{"plain", "zstd", "gzip-opaque"} {
		test.Run(representation, func(test *testing.T) {
			stored, _ := legacyCaptureGeometryStored(test, representation, bytes.Repeat([]byte("x"), 1<<20+17))
			original, parts := legacyCaptureDecodeFixture(test, stored)
			work := legacyCaptureDecodePrepare(test, original, parts, len(stored))
			for step := 0; step < 2; step++ {
				for _, factory := range legacyCaptureGeometryFactories(work) {
					source, err := factory.source(context.Background())
					if err == nil || source.Read != nil || len(source.Parts) != 0 || source.Bytes != 0 {
						test.Fatal("unfinished decode exposed a capture source", factory.name, step, err)
					}
				}
				if step == 0 {
					if complete, err := work.Step(context.Background()); complete || err != nil {
						test.Fatal("capture did not preserve its output quantum", complete, err)
					}
				}
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			before, err := work.RetainedResources()
			if err != nil {
				test.Fatal(err)
			}
			if complete, err := work.Step(ctx); complete || !errors.Is(err, context.Canceled) {
				test.Fatal("canceled capture advanced", complete, err)
			}
			if after, err := work.RetainedResources(); err != nil || after != before {
				test.Fatal("canceled capture changed retained resources", before, after, err)
			}
			if err := legacyCaptureDecodeDrain(test, work); err != nil {
				test.Fatal(err)
			}
			var retained [2]domain.LegacyByteSource
			factories := legacyCaptureGeometryFactories(work)
			for index, factory := range factories {
				retained[index], err = factory.source(context.Background())
				if err != nil {
					test.Fatal(err)
				}
				if _, err := factory.source(ctx); !errors.Is(err, context.Canceled) {
					test.Fatal("capture source ignored its current creation context", factory.name, err)
				}
				if _, err := retained[index].Read(ctx, 0, 1); !errors.Is(err, context.Canceled) {
					test.Fatal("capture source reused an earlier read context", factory.name, err)
				}
			}
			path := work.decoded.file.Name()
			if err := work.Close(); err != nil {
				test.Fatal(err)
			}
			if err := work.Close(); err != nil {
				test.Fatal("capture Close is not idempotent", err)
			}
			if work.reader != nil || work.decoded != nil || work.parts != nil || work.normalizationParts != nil || work.partHeap != (rootPublicationHeapEstimate{}) || work.stream != nil || work.closeStream != nil {
				test.Fatal("capture Close retained a manifest, hash ledger, spool, or stream")
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				test.Fatal("decoded spool survived capture Close", err)
			}
			for index, factory := range factories {
				if _, err := factory.source(context.Background()); !errors.Is(err, os.ErrClosed) {
					test.Fatal("closed capture exposed a new source", factory.name, err)
				}
				if _, err := retained[index].Read(context.Background(), 0, 1); !errors.Is(err, os.ErrClosed) {
					test.Fatal("old source outlived the capture spool", factory.name, err)
				}
				if _, err := retained[index].Read(ctx, 0, 1); !errors.Is(err, context.Canceled) {
					test.Fatal("closed source displaced current cancellation", factory.name, err)
				}
			}
			if resources, err := work.RetainedResources(); !errors.Is(err, os.ErrClosed) || resources != (outbound.RootPublicationResources{}) {
				test.Fatal("closed capture retained resource authority", resources, err)
			}
			if body, err := original.Read(context.Background(), 0, len(stored)); err != nil || !bytes.Equal(body, stored) {
				test.Fatal("capture Close consumed its borrowed original spool", err)
			}
		})
	}
}

func legacyCaptureGeometryConsumer(test *testing.T, source domain.LegacyByteSource, want domain.ContentHash) func(context.Context) (bool, error) {
	test.Helper()
	if source.PartBytes == domain.LegacyNormalizationPartBytes {
		decoder, err := domain.NewLegacyNormalizationDecoder(context.Background(), source)
		if err != nil {
			test.Fatal(err)
		}
		return func(ctx context.Context) (bool, error) {
			step, err := decoder.Step(ctx)
			return step.Complete, err
		}
	}
	target, err := newLegacyDocSpool()
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(func() {
		if err := target.Close(); err != nil {
			test.Error(err)
		}
	})
	preparation, err := domain.NewLegacyByteSourcePreparation(context.Background(), want, source, &legacyProofSpool{legacyDocSpool: target}, &domain.CanonicalDocVerifier{})
	if err != nil {
		test.Fatal(err)
	}
	return func(ctx context.Context) (bool, error) {
		progress, err := preparation.Step(ctx)
		return progress != domain.LegacyByteSourcePending, err
	}
}

func TestLegacyCaptureGeometryCurrentSpoolDamageRejected(test *testing.T) {
	body := append([]byte(`{"envelope":{"cir_version":"2"},"events":[],"ignored":"`), bytes.Repeat([]byte("x"), 1<<20+17)...)
	body = append(body, '"', '}')
	for _, representation := range []string{"plain", "zstd", "gzip-opaque"} {
		for geometry, name := range []string{"proof", "normalization"} {
			for _, damage := range []string{"subpart-seam", "proof-seam", "tail", "truncate", "extra", "cancel", "close"} {
				test.Run(representation+"/"+name+"/"+damage, func(test *testing.T) {
					stored, want := legacyCaptureGeometryStored(test, representation, body)
					original, parts := legacyCaptureDecodeFixture(test, stored)
					work := legacyCaptureDecodePrepare(test, original, parts, len(stored))
					if err := legacyCaptureDecodeDrain(test, work); err != nil {
						test.Fatal(err)
					}
					factory := legacyCaptureGeometryFactories(work)[geometry]
					source, err := factory.source(context.Background())
					if err != nil {
						test.Fatal(err)
					}
					checkLegacyCaptureGeometrySource(test, factory, source, want)
					step := legacyCaptureGeometryConsumer(test, source, domain.HashContent(want))
					ctx, cancel := context.WithCancel(context.Background())
					defer cancel()
					expected := domain.ErrPreparedDocRangeObject
					var mutation error
					switch damage {
					case "subpart-seam", "proof-seam", "tail":
						offset := len(want) - 1
						if damage == "subpart-seam" {
							offset = domain.LegacyNormalizationPartBytes + 7
						} else if damage == "proof-seam" {
							offset = domain.ConversationManifestChunkBytes + 7
						}
						_, mutation = work.decoded.file.WriteAt([]byte{want[offset] ^ 0xff}, int64(offset))
					case "truncate":
						mutation = work.decoded.file.Truncate(int64(work.decoded.size - 1))
					case "extra":
						_, mutation = work.decoded.file.WriteAt([]byte("!"), int64(work.decoded.size))
					case "cancel":
						cancel()
						expected = context.Canceled
					case "close":
						mutation = work.Close()
						expected = os.ErrClosed
					}
					if mutation != nil {
						test.Fatal(mutation)
					}
					if decoded, err := work.Bytes(ctx); !errors.Is(err, expected) || len(decoded) != 0 {
						test.Fatal("original proof readback admitted damaged or revoked decoded bytes", damage, len(decoded), err)
					}
					if damage == "truncate" || damage == "extra" {
						if _, err := source.Read(ctx, 0, 1); !errors.Is(err, domain.ErrPreparedDocRangeObject) {
							test.Fatal("source read ignored a changed physical spool extent", damage, err)
						}
						if resources, err := work.RetainedResources(); !errors.Is(err, domain.ErrPreparedDocRangeObject) || resources != (outbound.RootPublicationResources{}) {
							test.Fatal("resource ledger admitted a changed physical spool extent", damage, resources, err)
						}
					}
					for quantum := 0; quantum < 4*len(want)/domain.LegacyNormalizationPartBytes+64; quantum++ {
						complete, err := step(ctx)
						if err != nil {
							if !errors.Is(err, expected) {
								test.Fatal("source damage changed authenticated error precedence", damage, err, expected)
							}
							return
						}
						if complete {
							test.Fatal("damaged capture source became complete", damage)
						}
					}
					test.Fatal("damaged capture source did not reach bounded rejection", damage)
				})
			}
		}
	}
}

func checkLegacyCaptureGeometryResources(test *testing.T, work *legacyCaptureDecodeWork) {
	test.Helper()
	var hashes rootPublicationHeapEstimate
	for _, manifest := range [][]legacyCapturePart{work.parts, work.normalizationParts} {
		for _, part := range manifest {
			hashes.allocation(int64(len(part.hash)), 1)
		}
	}
	if hashes.err != nil || work.partHeap.bytes < hashes.bytes {
		test.Fatal("capture hash ledger omitted a retained manifest string", work.partHeap, hashes)
	}
	minimum := hashes
	minimum.allocation(1, int64(unsafe.Sizeof(*work)+unsafe.Sizeof(*work.reader)+unsafe.Sizeof(*work.decoded)))
	minimum.allocation(int64(cap(work.parts)), int64(unsafe.Sizeof(legacyCapturePart{})))
	minimum.allocation(int64(cap(work.normalizationParts)), int64(unsafe.Sizeof(legacyCapturePart{})))
	minimum.allocation(int64(cap(work.reader.buffer)), 1)
	minimum.allocation(int64(len(work.decoded.file.Name())), 1)
	info, err := work.decoded.file.Stat()
	if err != nil {
		test.Fatal(err)
	}
	resources, err := work.RetainedResources()
	if err != nil || minimum.err != nil || resources.HeapBytes < minimum.bytes || resources.HeapBytes > 2<<20 ||
		resources.SpoolBytes != info.Size() || resources.SpoolBytes != int64(work.decoded.size) || resources.KeepPermit != (work.compressed && work.stream != nil) {
		test.Fatal("capture omitted actual manifest capacity, hashes, spool bytes, or decoder permit", resources, minimum, err)
	}
	if len(work.parts) != (work.decoded.size+domain.ConversationManifestChunkBytes-1)/domain.ConversationManifestChunkBytes ||
		len(work.normalizationParts) != (work.decoded.size+domain.LegacyNormalizationPartBytes-1)/domain.LegacyNormalizationPartBytes {
		test.Fatal("decode quantum retained incomplete manifest geometry", work.decoded.size, len(work.parts), len(work.normalizationParts))
	}
}

func TestLegacyCaptureGeometryRetainsBothActualManifestCapacities(test *testing.T) {
	for _, representation := range []string{"plain", "zstd", "gzip-opaque"} {
		test.Run(representation, func(test *testing.T) {
			stored, _ := legacyCaptureGeometryStored(test, representation, bytes.Repeat([]byte("x"), 3<<20+17))
			original, parts := legacyCaptureDecodeFixture(test, stored)
			work := legacyCaptureDecodePrepare(test, original, parts, len(stored))
			complete := false
			for quantum := 0; quantum < 200; quantum++ {
				checkLegacyCaptureGeometryResources(test, work)
				before := work.decoded.size
				var err error
				complete, err = work.Step(context.Background())
				if err != nil || work.decoded.size-before > domain.ConversationManifestChunkBytes {
					test.Fatal("dual manifests changed the original decode quantum", quantum, err)
				}
				checkLegacyCaptureGeometryResources(test, work)
				if complete {
					break
				}
			}
			if !complete {
				test.Fatal("capture did not reach a bounded terminal result")
			}
			for _, manifest := range []*[]legacyCapturePart{&work.parts, &work.normalizationParts} {
				before, err := work.RetainedResources()
				if err != nil {
					test.Fatal(err)
				}
				const extra = 4096
				grown := make([]legacyCapturePart, len(*manifest), cap(*manifest)+extra)
				copy(grown, *manifest)
				*manifest = grown
				after, err := work.RetainedResources()
				if err != nil || after.HeapBytes-before.HeapBytes < int64(extra)*int64(unsafe.Sizeof(legacyCapturePart{})) || after.SpoolBytes != before.SpoolBytes || after.KeepPermit != before.KeepPermit {
					test.Fatal("capture ledger charged manifest length instead of retained capacity", before, after, err)
				}
				checkLegacyCaptureGeometryResources(test, work)
			}
		})
	}
}
