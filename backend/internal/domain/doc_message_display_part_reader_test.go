package domain

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestDocMessageDisplayPartReaderParityAndEviction(test *testing.T) {
	fixture := messageReadWorkFixture(test, DocMessageDisplayPageParts+1)
	if fixture.plan.Count != DocMessageDisplayPageParts+1 || fixture.plan.Parts.Count != 2 {
		test.Fatal("fixture must contain 64+1 display parts")
	}
	reader := docMessageDisplayPartReader{plan: fixture.plan}
	var first, previous *DocMessageDisplayPart
	previousIndex := -1
	for _, index := range []int{0, 0, 1, 1, 0} {
		var publicLoads, privateLoads []ContentHash
		public, publicErr := ReadDocMessageDisplayParts(context.Background(), fixture.plan, index, func(_ context.Context, hash ContentHash) ([]byte, error) {
			publicLoads = append(publicLoads, hash)
			return fixture.objects[hash], nil
		})
		parts, err := reader.read(context.Background(), index, func(_ context.Context, hash ContentHash) ([]byte, error) {
			privateLoads = append(privateLoads, hash)
			return fixture.objects[hash], nil
		})
		if err != nil || publicErr != nil || !reflect.DeepEqual(parts, public) || len(parts) != min(DocMessageDisplayPageParts, fixture.plan.Count-index*DocMessageDisplayPageParts) || !reflect.DeepEqual(privateLoads, publicLoads) || len(privateLoads) != 2 {
			test.Fatal("cold/warm part parity or callback sequence changed", index, privateLoads, publicLoads, err, publicErr)
		}
		if reader.cachedPlan != fixture.plan || reader.index != index || privateLoads[0] != fixture.plan.Parts.Hash || reader.hash != privateLoads[1] || !reflect.DeepEqual(reader.parts, parts) {
			test.Fatal("parsed parts not bound to full plan, index and resolved hash")
		}
		proof := &reader.parts[0]
		if proof == &public[0] || (index == previousIndex && proof != previous) || (index != previousIndex && proof == previous) || (index == 0 && previousIndex == 1 && proof == first) {
			test.Fatal("part proof reuse, eviction or standalone ownership changed", index)
		}
		if first == nil {
			first = proof
		}
		previous, previousIndex = proof, index
	}
}

func TestDocMessageDisplayPartReaderPlanKeyAndTextBounds(test *testing.T) {
	fixture := messageReadWorkFixture(test, DocMessageDisplayPageParts+1)
	load := func(_ context.Context, hash ContentHash) ([]byte, error) { return fixture.objects[hash], nil }
	reader := docMessageDisplayPartReader{plan: fixture.plan}
	if _, err := reader.read(context.Background(), 0, load); err != nil {
		test.Fatal(err)
	}
	proof, hash := &reader.parts[0], reader.hash
	last := reader.parts[len(reader.parts)-1]
	reader.plan.TextBytes++
	if parts, err := reader.read(context.Background(), 0, load); err != nil || len(parts) != DocMessageDisplayPageParts || reader.cachedPlan != reader.plan || reader.hash != hash || &reader.parts[0] == proof {
		test.Fatal("same page hash reused a different TextBytes plan's proof", err)
	}
	proof, cached := &reader.parts[0], reader.cachedPlan
	for _, scenario := range []struct {
		name   string
		index  int
		mutate func(*DocMessageDisplayPlan)
		loads  int
	}{
		{"part-past-total", 0, func(plan *DocMessageDisplayPlan) { plan.TextBytes = last.end() - 1 }, 2},
		{"insufficient-remaining-text", 0, func(plan *DocMessageDisplayPlan) { plan.TextBytes = last.end() + 1 }, 2},
		{"final-part-endpoint", 1, func(plan *DocMessageDisplayPlan) { plan.TextBytes++ }, 2},
		{"foreign-source", 0, func(plan *DocMessageDisplayPlan) {
			plan.Source.Input.Source.EventHash = HashContent([]byte("foreign message"))
		}, 2},
		{"invalid-plan", 0, func(plan *DocMessageDisplayPlan) { plan.Version++ }, 0},
	} {
		reader.plan = fixture.plan
		scenario.mutate(&reader.plan)
		loads := 0
		parts, err := reader.read(context.Background(), scenario.index, func(ctx context.Context, hash ContentHash) ([]byte, error) {
			loads++
			return load(ctx, hash)
		})
		if !errors.Is(err, ErrIntegrity) || parts != nil || loads != scenario.loads || reader.cachedPlan != cached || reader.index != 0 || reader.hash != hash || len(reader.parts) != DocMessageDisplayPageParts || &reader.parts[0] != proof {
			test.Fatal("plan binding or text bounds admitted invalid proof or evicted valid proof", scenario.name, loads, err)
		}
	}
	reader.plan = cached
	if _, err := reader.read(context.Background(), 0, load); err != nil || &reader.parts[0] != proof {
		test.Fatal("failed plan admission replaced the prior parsed proof", err)
	}
}

func TestDocMessageDisplayPartReaderOwnsProofAndPreservesItOnFailure(test *testing.T) {
	fixture := messageReadWorkFixture(test, DocMessageDisplayPageParts+1)
	load := func(_ context.Context, hash ContentHash) ([]byte, error) { return fixture.objects[hash], nil }
	reader := docMessageDisplayPartReader{plan: fixture.plan}
	if _, err := reader.read(context.Background(), 0, load); err != nil {
		test.Fatal(err)
	}
	proof, hash := &reader.parts[0], reader.hash
	want, err := ReadDocMessageDisplayParts(context.Background(), fixture.plan, 0, func(_ context.Context, hash ContentHash) ([]byte, error) {
		return bytes.Clone(fixture.objects[hash]), nil
	})
	if err != nil {
		test.Fatal(err)
	}
	raw := fixture.objects[hash]
	original := bytes.Clone(raw)
	for offset := range raw {
		raw[offset] = '!'
	}
	if !reflect.DeepEqual(reader.parts, want) {
		test.Fatal("parsed part strings alias loader-owned raw bytes")
	}
	loadFailure := errors.New("display part load failed")
	for _, scenario := range []struct {
		name      string
		index     int
		cancel    bool
		fail      bool
		corrupt   bool
		wantError error
	}{
		{"mutated-cache-hit", 0, false, false, false, ErrIntegrity},
		{"load-error", 0, false, true, false, loadFailure},
		{"cancel-cache-hit-error", 0, true, true, false, context.Canceled},
		{"cancel-new-page", 1, true, false, false, context.Canceled},
		{"invalid-new-page", 1, false, false, true, ErrIntegrity},
	} {
		ctx, cancel := context.WithCancel(context.Background())
		loads := 0
		parts, err := reader.read(ctx, scenario.index, func(_ context.Context, hash ContentHash) ([]byte, error) {
			loads++
			if loads == 2 {
				if scenario.cancel {
					cancel()
				}
				if scenario.fail {
					return fixture.objects[hash], loadFailure
				}
				if scenario.corrupt {
					return []byte("corrupt"), nil
				}
			}
			return fixture.objects[hash], nil
		})
		cancel()
		if !errors.Is(err, scenario.wantError) || parts != nil || loads != 2 || reader.cachedPlan != fixture.plan || reader.index != 0 || reader.hash != hash || !reflect.DeepEqual(reader.parts, want) || &reader.parts[0] != proof {
			test.Fatal("failed/cancelled read exposed parts or replaced valid proof", scenario.name, loads, err)
		}
		copy(raw, original)
	}
	if parts, err := reader.read(context.Background(), 0, load); err != nil || !reflect.DeepEqual(parts, want) || &reader.parts[0] != proof {
		test.Fatal("restored raw did not reuse the valid parsed proof", err)
	}
}
