package domain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

type docRangePrefixFixture struct {
	total       int
	offset      int
	edit        func(int, []DocRangeEntry) []DocRangeEntry
	hashPages   []int
	readPages   []int
	readEntries int
}

func (fixture *docRangePrefixFixture) page(page int) []DocRangeEntry {
	start := page * DocRangePageEvents
	entries := make([]DocRangeEntry, min(DocRangePageEvents, fixture.total-start))
	for ordinal := range entries {
		position := start + ordinal
		body := []byte(fmt.Sprintf(`{"kind":"message","role":"user","seq":%d}`, position))
		entries[ordinal] = DocRangeEntry{
			DocEventIndex: DocEventIndex{Index: position, Offset: fixture.offset + position*128, Length: len(body), Hash: HashContent(body), Seq: position, Role: "user"},
			Kind:          EventMessage,
		}
	}
	if fixture.edit != nil {
		entries = fixture.edit(page, entries)
	}
	return entries
}

func (fixture *docRangePrefixFixture) source(test testing.TB) DocRangePrefixSource {
	test.Helper()
	checkPage := func(page int) {
		test.Helper()
		if page < 0 || page >= (fixture.total+DocRangePageEvents-1)/DocRangePageEvents {
			test.Fatalf("page %d outside source with %d events", page, fixture.total)
		}
	}
	return DocRangePrefixSource{
		Total: fixture.total,
		PageHash: func(_ context.Context, page int) (ContentHash, error) {
			checkPage(page)
			fixture.hashPages = append(fixture.hashPages, page)
			raw, err := json.Marshal(fixture.page(page))
			if err != nil {
				test.Fatal(err)
			}
			return HashContent(raw), nil
		},
		ReadPage: func(_ context.Context, page int) ([]DocRangeEntry, error) {
			checkPage(page)
			fixture.readPages = append(fixture.readPages, page)
			entries := fixture.page(page)
			fixture.readEntries += len(entries)
			return entries, nil
		},
	}
}

func docRangePrefixAssertPages(test testing.TB, actual []int, start, count int) {
	test.Helper()
	if len(actual) != count {
		test.Fatalf("visited pages %v; want %d pages starting at %d", actual, count, start)
	}
	for ordinal, page := range actual {
		if page != start+ordinal {
			test.Fatalf("visited pages %v; want consecutive pages starting at %d", actual, start)
		}
	}
}

func docRangePrefixAssertError(test testing.TB, progress DocRangePrefixProgress, err, expected error) {
	test.Helper()
	if !errors.Is(err, expected) || progress != (DocRangePrefixProgress{}) {
		test.Fatalf("progress %+v, error %v; want zero progress and %v", progress, err, expected)
	}
}

func TestCompareDocRangePrefixResumesBoundedBatches(test *testing.T) {
	test.Parallel()
	for _, offset := range []int{0, 4096} {
		for _, budget := range []int{1, 3, DocRangePrefixBatchPages} {
			test.Run(fmt.Sprintf("offset=%d/budget=%d", offset, budget), func(test *testing.T) {
				total := (DocRangePrefixBatchPages+2)*DocRangePageEvents + 7
				left := &docRangePrefixFixture{total: total}
				right := &docRangePrefixFixture{total: total, offset: offset}
				leftSource, rightSource := left.source(test), right.source(test)
				for start := 0; start < total; {
					progress, err := CompareDocRangePrefix(context.Background(), leftSource, rightSource, start, budget)
					end := min(total, start+budget*DocRangePageEvents)
					expected := DocRangePrefixProgress{Events: end, Complete: end == total}
					if err != nil || progress != expected {
						test.Fatalf("start %d: progress %+v, error %v; want %+v", start, progress, err, expected)
					}
					pages := (end - start + DocRangePageEvents - 1) / DocRangePageEvents
					for _, fixture := range []*docRangePrefixFixture{left, right} {
						docRangePrefixAssertPages(test, fixture.hashPages, start/DocRangePageEvents, pages)
						reads := 0
						if offset != 0 {
							reads = pages
						}
						docRangePrefixAssertPages(test, fixture.readPages, start/DocRangePageEvents, reads)
						fixture.hashPages, fixture.readPages = nil, nil
					}
					start = progress.Events
				}
			})
		}
	}
}

func TestCompareDocRangePrefixFindsExactFirstMismatch(test *testing.T) {
	test.Parallel()
	total := 6*DocRangePageEvents + 17
	for _, page := range []int{0, 3, 6} {
		count := min(DocRangePageEvents, total-page*DocRangePageEvents)
		for _, ordinal := range []int{0, count / 2, count - 1} {
			for _, budget := range []int{1, DocRangePrefixBatchPages} {
				for _, side := range []string{"left", "right"} {
					test.Run(fmt.Sprintf("page=%d/event=%d/budget=%d/%s", page, ordinal, budget, side), func(test *testing.T) {
						left := &docRangePrefixFixture{total: total}
						right := &docRangePrefixFixture{total: total}
						changed := right
						if side == "left" {
							changed = left
						}
						changed.edit = func(position int, entries []DocRangeEntry) []DocRangeEntry {
							if position == page {
								entries[ordinal].Hash = HashContent([]byte(fmt.Sprintf(`{"kind":"message","role":"assistant","seq":%d}`, entries[ordinal].Index)))
								if ordinal+1 < len(entries) {
									entries[ordinal+1].Hash = ""
								}
							}
							return entries
						}
						leftSource, rightSource := left.source(test), right.source(test)
						mismatch := page*DocRangePageEvents + ordinal
						for start := 0; ; {
							progress, err := CompareDocRangePrefix(context.Background(), leftSource, rightSource, start, budget)
							end := min(start+budget*DocRangePageEvents, mismatch)
							complete := mismatch < start+budget*DocRangePageEvents
							if err != nil || progress != (DocRangePrefixProgress{Events: end, Complete: complete}) {
								test.Fatalf("start %d: progress %+v, error %v; want events %d, complete %t", start, progress, err, end, complete)
							}
							if complete {
								break
							}
							start = progress.Events
						}
						for _, fixture := range []*docRangePrefixFixture{left, right} {
							docRangePrefixAssertPages(test, fixture.hashPages, 0, page+1)
							docRangePrefixAssertPages(test, fixture.readPages, page, 1)
						}
					})
				}
			}
		}
	}
}

func TestCompareDocRangePrefixEmptyAndFinalPartialCounts(test *testing.T) {
	test.Parallel()
	for _, counts := range [][2]int{
		{0, 0}, {0, 7}, {7, 0}, {1, 1}, {DocRangePageEvents, DocRangePageEvents},
		{DocRangePageEvents + 7, DocRangePageEvents + 7},
		{DocRangePageEvents + 7, DocRangePageEvents + 11},
		{DocRangePageEvents + 11, DocRangePageEvents + 7},
		{DocRangePageEvents, DocRangePageEvents + 7},
		{DocRangePageEvents + 7, DocRangePageEvents},
		{DocRangePageEvents + 7, 3 * DocRangePageEvents},
		{3 * DocRangePageEvents, DocRangePageEvents + 7},
	} {
		for _, offset := range []int{0, 4096} {
			test.Run(fmt.Sprintf("left=%d/right=%d/offset=%d", counts[0], counts[1], offset), func(test *testing.T) {
				total := min(counts[0], counts[1])
				left := &docRangePrefixFixture{total: counts[0]}
				right := &docRangePrefixFixture{total: counts[1], offset: offset}
				for _, fixture := range []*docRangePrefixFixture{left, right} {
					fixture.edit = func(_ int, entries []DocRangeEntry) []DocRangeEntry {
						for ordinal := range entries {
							if entries[ordinal].Index >= total {
								entries[ordinal].Hash = HashContent([]byte("different suffix"))
							}
						}
						return entries
					}
				}
				leftSource, rightSource := left.source(test), right.source(test)
				progress, err := CompareDocRangePrefix(context.Background(), leftSource, rightSource, 0, DocRangePrefixBatchPages)
				if err != nil || progress != (DocRangePrefixProgress{Events: total, Complete: true}) {
					test.Fatalf("progress %+v, error %v; want complete prefix %d", progress, err, total)
				}
				pages := (total + DocRangePageEvents - 1) / DocRangePageEvents
				for _, fixture := range []*docRangePrefixFixture{left, right} {
					docRangePrefixAssertPages(test, fixture.hashPages, 0, pages)
					if offset != 0 {
						docRangePrefixAssertPages(test, fixture.readPages, 0, pages)
					} else if counts[0] != counts[1] && total%DocRangePageEvents != 0 {
						docRangePrefixAssertPages(test, fixture.readPages, pages-1, 1)
					} else {
						docRangePrefixAssertPages(test, fixture.readPages, 0, 0)
					}
					fixture.hashPages, fixture.readPages = nil, nil
				}
				progress, err = CompareDocRangePrefix(context.Background(), leftSource, rightSource, total, 1)
				if err != nil || progress != (DocRangePrefixProgress{Events: total, Complete: true}) {
					test.Fatalf("completed cursor: progress %+v, error %v", progress, err)
				}
				for _, fixture := range []*docRangePrefixFixture{left, right} {
					docRangePrefixAssertPages(test, fixture.hashPages, 0, 0)
					docRangePrefixAssertPages(test, fixture.readPages, 0, 0)
				}
			})
		}
	}
}

func TestCompareDocRangePrefixRejectsInvalidInputsBeforeCallbacks(test *testing.T) {
	test.Parallel()
	total := 2*DocRangePageEvents + 7
	for _, scenario := range []struct {
		name        string
		leftTotal   int
		rightTotal  int
		start       int
		budget      int
		nilCallback string
	}{
		{"negative-left-total", -1, total, 0, 1, ""},
		{"negative-right-total", total, -1, 0, 1, ""},
		{"oversized-left-total", MaxConversationDocumentBytes + 1, total, 0, 1, ""},
		{"oversized-right-total", total, MaxConversationDocumentBytes + 1, 0, 1, ""},
		{"negative-start", total, total, -1, 1, ""},
		{"past-left-total", 7, total, DocRangePageEvents, 1, ""},
		{"past-right-total", total, 7, DocRangePageEvents, 1, ""},
		{"unaligned-first-page", total, total, 1, 1, ""},
		{"unaligned-later-page", total, total, DocRangePageEvents + 1, 1, ""},
		{"past-empty", 0, 0, 1, 1, ""},
		{"zero-budget", total, total, 0, 0, ""},
		{"negative-budget", total, total, 0, -1, ""},
		{"over-budget", total, total, 0, DocRangePrefixBatchPages + 1, ""},
		{"empty-zero-budget", 0, 0, 0, 0, ""},
		{"completed-zero-budget", total, total, total, 0, ""},
		{"nil-left-hash", total, total, 0, 1, "left-hash"},
		{"nil-right-hash", total, total, 0, 1, "right-hash"},
		{"nil-left-read", total, total, 0, 1, "left-read"},
		{"nil-right-read", total, total, 0, 1, "right-read"},
		{"empty-nil-left-hash", 0, 0, 0, 1, "left-hash"},
		{"empty-nil-right-hash", 0, 0, 0, 1, "right-hash"},
		{"empty-nil-left-read", 0, 0, 0, 1, "left-read"},
		{"empty-nil-right-read", 0, 0, 0, 1, "right-read"},
		{"zero-sources", 0, 0, 0, 1, "all"},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			left := &docRangePrefixFixture{total: scenario.leftTotal}
			right := &docRangePrefixFixture{total: scenario.rightTotal}
			leftSource, rightSource := left.source(test), right.source(test)
			switch scenario.nilCallback {
			case "left-hash":
				leftSource.PageHash = nil
			case "right-hash":
				rightSource.PageHash = nil
			case "left-read":
				leftSource.ReadPage = nil
			case "right-read":
				rightSource.ReadPage = nil
			case "all":
				leftSource, rightSource = DocRangePrefixSource{}, DocRangePrefixSource{}
			}
			progress, err := CompareDocRangePrefix(context.Background(), leftSource, rightSource, scenario.start, scenario.budget)
			docRangePrefixAssertError(test, progress, err, ErrValidation)
			for _, fixture := range []*docRangePrefixFixture{left, right} {
				docRangePrefixAssertPages(test, fixture.hashPages, 0, 0)
				docRangePrefixAssertPages(test, fixture.readPages, 0, 0)
			}
		})
	}
}

func TestCompareDocRangePrefixRejectsMalformedHashes(test *testing.T) {
	test.Parallel()
	for _, malformed := range []ContentHash{
		"", "sha256:short", ContentHash("sha256:" + strings.Repeat("a", 65)),
		ContentHash("sha256:" + strings.Repeat("A", 64)),
		ContentHash("sha256:" + strings.Repeat("g", 64)),
		ContentHash("sha512:" + strings.Repeat("a", 64)),
	} {
		for _, side := range []string{"left", "right", "both"} {
			for _, ordinal := range []int{-1, 0, DocRangePageEvents / 2, DocRangePageEvents - 1} {
				test.Run(fmt.Sprintf("hash=%s/%s/event=%d", malformed, side, ordinal), func(test *testing.T) {
					left := &docRangePrefixFixture{total: 3 * DocRangePageEvents}
					right := &docRangePrefixFixture{total: left.total}
					if ordinal >= 0 {
						right.offset = 4096
					}
					leftSource, rightSource := left.source(test), right.source(test)
					for _, target := range []struct {
						name    string
						fixture *docRangePrefixFixture
						source  *DocRangePrefixSource
					}{{"left", left, &leftSource}, {"right", right, &rightSource}} {
						if side != "both" && side != target.name {
							continue
						}
						if ordinal < 0 {
							original := target.source.PageHash
							target.source.PageHash = func(ctx context.Context, page int) (ContentHash, error) {
								hash, err := original(ctx, page)
								if page == 1 {
									return malformed, nil
								}
								return hash, err
							}
						} else {
							target.fixture.edit = func(page int, entries []DocRangeEntry) []DocRangeEntry {
								if page == 1 {
									entries[ordinal].Hash = malformed
								}
								return entries
							}
						}
					}
					progress, err := CompareDocRangePrefix(context.Background(), leftSource, rightSource, 0, DocRangePrefixBatchPages)
					docRangePrefixAssertError(test, progress, err, ErrIntegrity)
					for _, fixture := range []*docRangePrefixFixture{left, right} {
						docRangePrefixAssertPages(test, fixture.hashPages, 0, 2)
						reads := 0
						if ordinal >= 0 {
							reads = 2
						}
						docRangePrefixAssertPages(test, fixture.readPages, 0, reads)
					}
				})
			}
		}
	}
}

func TestCompareDocRangePrefixRejectsWrongPageLengths(test *testing.T) {
	test.Parallel()
	for _, scenario := range []struct {
		name   string
		total  int
		length int
	}{
		{"full-empty", 3 * DocRangePageEvents, 0},
		{"full-short", 3 * DocRangePageEvents, DocRangePageEvents - 1},
		{"full-long", 3 * DocRangePageEvents, DocRangePageEvents + 1},
		{"partial-empty", DocRangePageEvents + 7, 0},
		{"partial-short", DocRangePageEvents + 7, 6},
		{"partial-long", DocRangePageEvents + 7, 8},
		{"partial-full", DocRangePageEvents + 7, DocRangePageEvents},
	} {
		for _, side := range []string{"left", "right"} {
			test.Run(scenario.name+"/"+side, func(test *testing.T) {
				left := &docRangePrefixFixture{total: 3 * DocRangePageEvents}
				right := &docRangePrefixFixture{total: 3 * DocRangePageEvents, offset: 4096}
				changed := right
				if side == "left" {
					changed = left
				}
				changed.total = scenario.total
				changed.edit = func(page int, entries []DocRangeEntry) []DocRangeEntry {
					if page != 1 {
						return entries
					}
					resized := make([]DocRangeEntry, scenario.length)
					for ordinal := range resized {
						resized[ordinal] = entries[ordinal%len(entries)]
					}
					return resized
				}
				progress, err := CompareDocRangePrefix(context.Background(), left.source(test), right.source(test), 0, DocRangePrefixBatchPages)
				docRangePrefixAssertError(test, progress, err, ErrIntegrity)
				for _, fixture := range []*docRangePrefixFixture{left, right} {
					docRangePrefixAssertPages(test, fixture.hashPages, 0, 2)
					docRangePrefixAssertPages(test, fixture.readPages, 0, 2)
				}
			})
		}
	}
}

func TestCompareDocRangePrefixPropagatesCallbackErrors(test *testing.T) {
	test.Parallel()
	operations := []string{"left-hash", "right-hash", "left-read", "right-read"}
	for _, failurePage := range []int{0, 1} {
		for _, operation := range operations {
			test.Run(fmt.Sprintf("page=%d/%s", failurePage, operation), func(test *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				failure := errors.New("synthetic callback failure")
				wrapped := fmt.Errorf("source: %w", failure)
				var calls []string
				observe := func(callbackCtx context.Context, name string, page int) error {
					if callbackCtx != ctx {
						test.Fatal("callback did not receive comparison context")
					}
					calls = append(calls, fmt.Sprintf("%s:%d", name, page))
					if name == operation && page == failurePage {
						return wrapped
					}
					return nil
				}
				wrap := func(name string, source DocRangePrefixSource) DocRangePrefixSource {
					return DocRangePrefixSource{
						Total: source.Total,
						PageHash: func(callbackCtx context.Context, page int) (ContentHash, error) {
							if err := observe(callbackCtx, name+"-hash", page); err != nil {
								return "", err
							}
							return source.PageHash(callbackCtx, page)
						},
						ReadPage: func(callbackCtx context.Context, page int) ([]DocRangeEntry, error) {
							if err := observe(callbackCtx, name+"-read", page); err != nil {
								return nil, err
							}
							return source.ReadPage(callbackCtx, page)
						},
					}
				}
				left := &docRangePrefixFixture{total: 3 * DocRangePageEvents}
				right := &docRangePrefixFixture{total: left.total, offset: 4096}
				progress, err := CompareDocRangePrefix(ctx, wrap("left", left.source(test)), wrap("right", right.source(test)), 0, DocRangePrefixBatchPages)
				docRangePrefixAssertError(test, progress, err, failure)
				if !errors.Is(err, wrapped) {
					test.Fatalf("callback error lost its wrapper: %v", err)
				}
				var expected []string
				for page := 0; page <= failurePage; page++ {
					for _, name := range operations {
						expected = append(expected, fmt.Sprintf("%s:%d", name, page))
						if page == failurePage && name == operation {
							break
						}
					}
				}
				if !slices.Equal(calls, expected) {
					test.Fatalf("callback calls %v; want %v", calls, expected)
				}
			})
		}
	}
}

type docRangePrefixContext struct {
	context.Context
	checks   int
	cancelAt int
	cancel   context.CancelFunc
}

func (ctx *docRangePrefixContext) Err() error {
	ctx.checks++
	if ctx.cancel != nil && ctx.checks == ctx.cancelAt {
		ctx.cancel()
	}
	return ctx.Context.Err()
}

func TestCompareDocRangePrefixCancellationCheckpoints(test *testing.T) {
	test.Parallel()
	for _, offset := range []int{0, 4096} {
		for _, budget := range []int{2, 3} {
			test.Run(fmt.Sprintf("offset=%d/budget=%d", offset, budget), func(test *testing.T) {
				total := 2*DocRangePageEvents + 7
				baseline := &docRangePrefixContext{Context: context.Background()}
				left := &docRangePrefixFixture{total: total}
				right := &docRangePrefixFixture{total: total, offset: offset}
				if _, err := CompareDocRangePrefix(baseline, left.source(test), right.source(test), 0, budget); err != nil {
					test.Fatal(err)
				}
				checkpoints := []int{1, 2, 3, baseline.checks - 1, baseline.checks}
				if offset != 0 {
					checkpoints = append(checkpoints, 3+DocRangePageEvents/2, 3+DocRangePageEvents)
				}
				slices.Sort(checkpoints)
				for _, checkpoint := range slices.Compact(checkpoints) {
					test.Run(fmt.Sprintf("check=%d", checkpoint), func(test *testing.T) {
						base, cancel := context.WithCancel(context.Background())
						defer cancel()
						ctx := &docRangePrefixContext{Context: base, cancelAt: checkpoint, cancel: cancel}
						left := &docRangePrefixFixture{total: total}
						right := &docRangePrefixFixture{total: total, offset: offset}
						progress, err := CompareDocRangePrefix(ctx, left.source(test), right.source(test), 0, budget)
						docRangePrefixAssertError(test, progress, err, context.Canceled)
						if ctx.checks != checkpoint {
							test.Fatalf("continued checking after cancellation: %d checks, canceled at %d", ctx.checks, checkpoint)
						}
						if checkpoint == 1 && (len(left.hashPages) != 0 || len(right.hashPages) != 0) {
							test.Fatal("pre-canceled comparison called a source")
						}
					})
				}
			})
		}
	}
}

func TestCompareDocRangePrefixCancellationFromCallbacks(test *testing.T) {
	test.Parallel()
	for _, offset := range []int{0, 4096} {
		for _, operation := range []string{"left-hash", "right-hash", "left-read", "right-read"} {
			if offset == 0 && strings.HasSuffix(operation, "read") {
				continue
			}
			test.Run(fmt.Sprintf("offset=%d/%s", offset, operation), func(test *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				left := &docRangePrefixFixture{total: DocRangePageEvents + 7}
				right := &docRangePrefixFixture{total: left.total, offset: offset}
				leftSource, rightSource := left.source(test), right.source(test)
				source := &leftSource
				if strings.HasPrefix(operation, "right") {
					source = &rightSource
				}
				if strings.HasSuffix(operation, "hash") {
					original := source.PageHash
					source.PageHash = func(ctx context.Context, page int) (ContentHash, error) {
						hash, err := original(ctx, page)
						if page == 1 {
							cancel()
						}
						return hash, err
					}
				} else {
					original := source.ReadPage
					source.ReadPage = func(ctx context.Context, page int) ([]DocRangeEntry, error) {
						entries, err := original(ctx, page)
						if page == 1 {
							cancel()
						}
						return entries, err
					}
				}
				progress, err := CompareDocRangePrefix(ctx, leftSource, rightSource, 0, DocRangePrefixBatchPages)
				docRangePrefixAssertError(test, progress, err, context.Canceled)
			})
		}
	}
	for _, total := range []int{0, DocRangePageEvents + 7} {
		test.Run(fmt.Sprintf("deadline/total=%d", total), func(test *testing.T) {
			ctx, cancel := context.WithDeadline(context.Background(), time.Unix(1, 0))
			defer cancel()
			left := &docRangePrefixFixture{total: total}
			right := &docRangePrefixFixture{total: total}
			progress, err := CompareDocRangePrefix(ctx, left.source(test), right.source(test), 0, 1)
			docRangePrefixAssertError(test, progress, err, context.DeadlineExceeded)
			for _, fixture := range []*docRangePrefixFixture{left, right} {
				docRangePrefixAssertPages(test, fixture.hashPages, 0, 0)
				docRangePrefixAssertPages(test, fixture.readPages, 0, 0)
			}
		})
	}
}

func TestCompareDocRangePrefixFixedBatchWorkAtTenTimesTotal(test *testing.T) {
	test.Parallel()
	if DocRangePrefixBatchPages != 64 {
		test.Fatalf("batch page limit %d; want 64", DocRangePrefixBatchPages)
	}
	for _, offset := range []int{0, 4096} {
		for _, start := range []int{0, DocRangePrefixBatchPages * DocRangePageEvents} {
			for _, scale := range []int{1, 10} {
				test.Run(fmt.Sprintf("offset=%d/start=%d/scale=%d", offset, start, scale), func(test *testing.T) {
					total := scale * (2*DocRangePrefixBatchPages + 1) * DocRangePageEvents
					left := &docRangePrefixFixture{total: total}
					right := &docRangePrefixFixture{total: total, offset: offset}
					leftSource, rightSource := left.source(test), right.source(test)
					if offset == 0 {
						for _, source := range []*DocRangePrefixSource{&leftSource, &rightSource} {
							source.ReadPage = func(context.Context, int) ([]DocRangeEntry, error) {
								test.Fatal("read page body despite equal authenticated page hashes")
								return nil, ErrIntegrity
							}
						}
					}
					ctx := &docRangePrefixContext{Context: context.Background()}
					progress, err := CompareDocRangePrefix(ctx, leftSource, rightSource, start, DocRangePrefixBatchPages)
					expected := DocRangePrefixProgress{Events: start + DocRangePrefixBatchPages*DocRangePageEvents}
					if err != nil || progress != expected {
						test.Fatalf("progress %+v, error %v; want %+v", progress, err, expected)
					}
					reads, entries := 0, 0
					if offset != 0 {
						reads, entries = DocRangePrefixBatchPages, DocRangePrefixBatchPages*DocRangePageEvents
					}
					for _, fixture := range []*docRangePrefixFixture{left, right} {
						docRangePrefixAssertPages(test, fixture.hashPages, start/DocRangePageEvents, DocRangePrefixBatchPages)
						docRangePrefixAssertPages(test, fixture.readPages, start/DocRangePageEvents, reads)
						if fixture.readEntries != entries {
							test.Fatalf("read %d entries; want fixed batch work %d", fixture.readEntries, entries)
						}
					}
					if expectedChecks := DocRangePrefixBatchPages + entries + 2; ctx.checks != expectedChecks {
						test.Fatalf("context checks %d; want fixed batch work %d", ctx.checks, expectedChecks)
					}
				})
			}
		}
	}
}
