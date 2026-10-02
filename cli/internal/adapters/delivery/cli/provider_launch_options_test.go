package cli

import (
	"reflect"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

func TestProviderLaunchOptionsShareInvocationParser(t *testing.T) {
	req := ProviderLaunchRequest{Intent: LaunchIntent{Provider: domain.ProviderCodex, ProviderArgs: []string{"-mfoo", "--config=x=y", "-i", "one.png", "two.png", "--yolo", "--", "-p literal prompt"}}}
	details, err := req.ArgumentDetails()
	if err != nil {
		t.Fatal(err)
	}
	want := []ProviderLaunchOption{{Name: "-m", Values: []string{"foo"}}, {Name: "--config", Values: []string{"x=y"}}, {Name: "-i", Values: []string{"one.png", "two.png"}}, {Name: "--yolo"}}
	if !reflect.DeepEqual(details.Options, want) || !reflect.DeepEqual(details.Prompts, []string{"-p literal prompt"}) {
		t.Fatal("normalized options lost their original roles")
	}
	details.Options[2].Values[1] = "changed"
	details.Options[0].Name = "changed"
	again, err := req.ArgumentDetails()
	if err != nil || !reflect.DeepEqual(again.Options, want) {
		t.Fatal("returned option slice aliases original launch")
	}
}
