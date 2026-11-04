package documentquery

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestDocFindFragmentRootReauthorizesAfterInteractiveAdmission(test *testing.T) {
	assertVisibleReadAuthorizationAfterAdmission(test, func(ctx context.Context, queries *Queries, selection domain.DocVisibleSelector) error {
		page, err := queries.readRootDocFindFragment(ctx, selection, domain.DocFindFragmentRequest{Budget: 4})
		if page != (domain.DocFindFragmentPage{}) {
			return domain.ErrIntegrity
		}
		return err
	})
}

func TestDocFindFragmentRangeUsesBoundedPreviousProbeAndRejectsCorruption(test *testing.T) {
	raw := []byte(strings.Repeat("a\ud55c🙂é", 20_000))
	entry := domain.DocEventIndex{Length: len(raw), Hash: domain.HashContent(raw)}
	hash := domain.HashContent([]byte("document"))
	var lengths []int
	source := docViewSource{total: 1, location: func(int) (domain.DocEventIndex, error) { return entry, nil }, read: func(offset, length int) ([]byte, error) {
		lengths = append(lengths, length)
		return raw[offset : offset+length], nil
	}}
	offset := 100_000
	for _, budget := range []int{4, 7, 65535, 65536} {
		lengths = nil
		page, err := readDocFindSourceFragment(source, hash, "", 0, offset, budget)
		if err != nil || len(lengths) != 2 || lengths[0] != budget || lengths[1] != utf8.UTFMax || page.Previous < max(0, offset-budget-3) || page.Previous >= offset || !utf8.RuneStart(raw[page.Previous]) {
			test.Fatal("backward navigation scanned the event", page, err, lengths)
		}
	}
	for _, probe := range [][]byte{{0xff, 'a', 'a', 'a'}, {0x80, 0xff, 'a', 'a'}, {0xc0, 0xaf, 'a', 'a'}, {0x80, 0x80, 0x80, 0x80}} {
		lengths = nil
		source.read = func(start, length int) ([]byte, error) {
			lengths = append(lengths, length)
			if start == offset-65536 {
				return probe, nil
			}
			return raw[start : start+length], nil
		}
		page, err := readDocFindSourceFragment(source, hash, "", 0, offset, 65536)
		if !errors.Is(err, domain.ErrIntegrity) || page != (domain.DocFindFragmentPage{}) || !reflect.DeepEqual(lengths, []int{65536, utf8.UTFMax}) {
			test.Fatal("corrupt previous probe accepted or scanned beyond four bytes", page, err, lengths)
		}
	}
	for _, corrupt := range [][]byte{{'a', 0xff, 'b', 'c', 'd'}, {'a', 'b', 0xf0, 0x9f}, {'a', 0xc0, 0xaf, 'b'}} {
		entry.Length, entry.Hash = len(corrupt), domain.HashContent(corrupt)
		source.read = func(offset, length int) ([]byte, error) { return corrupt[offset : offset+length], nil }
		page, err := readDocFindSourceFragment(source, hash, "", 0, 0, 4)
		if !errors.Is(err, domain.ErrIntegrity) || page != (domain.DocFindFragmentPage{}) {
			test.Fatal("corrupt UTF-8 was silently truncated", page, err)
		}
	}
	entry.Length, entry.Hash = len(raw), domain.HashContent(raw)
	source.read = func(int, int) ([]byte, error) { return []byte("short"), nil }
	if page, err := readDocFindSourceFragment(source, hash, "", 0, 0, 65536); !errors.Is(err, domain.ErrIntegrity) || page != (domain.DocFindFragmentPage{}) {
		test.Fatal("short range accepted", page, err)
	}
}
