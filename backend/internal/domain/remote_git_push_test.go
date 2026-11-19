package domain

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestRemoteBranchPushesAreWhatChangedBetweenTwoReads(t *testing.T) {
	sha := func(letter string) string { return strings.Repeat(letter, 40) }
	at := time.Date(2031, 1, 2, 3, 0, 0, 0, time.FixedZone("local", 9*3600))
	before := []RemoteBranch{{Name: "main", SHA: sha("a")}, {Name: "kept", SHA: sha("b")}, {Name: "deleted", SHA: sha("c")}}
	after := []RemoteBranch{{Name: "kept", SHA: sha("b")}, {Name: "main", SHA: sha("d")}, {Name: "new", SHA: sha("e")}}
	want := []RemoteBranchPush{{Branch: "main", SHA: sha("d"), SeenAt: at.UTC()}, {Branch: "new", SHA: sha("e"), SeenAt: at.UTC()}}
	got := RemoteBranchPushes(before, after, at)
	if !reflect.DeepEqual(got, want) || got[0].SeenAt.Location() != time.UTC {
		t.Fatalf("pushes = %+v", got)
	}
	if same := RemoteBranchPushes(after, after, at); same == nil || len(same) != 0 {
		t.Fatalf("an unchanged list: %#v", same)
	}
	for _, push := range got {
		if err := push.Validate(); err != nil {
			t.Fatal(err)
		}
	}
	for name, push := range map[string]RemoteBranchPush{
		"no branch": {SHA: sha("a"), SeenAt: at}, "a short commit number": {Branch: "main", SHA: "abc", SeenAt: at}, "no time": {Branch: "main", SHA: sha("a")},
	} {
		if err := push.Validate(); !errors.Is(err, ErrValidation) {
			t.Errorf("%s was accepted: %v", name, err)
		}
	}
}
