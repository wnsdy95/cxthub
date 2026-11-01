package domain

import (
	"strings"
	"testing"
)

func TestConversationFindPreparationInputBounds(test *testing.T) {
	request := ConversationFindPreparationRequest{Version: 1, RepoID: HashContent([]byte("repo")), Hash: HashContent([]byte("document")), Find: ConversationFindRequest{Query: "needle"}, DocumentIdentities: []DocumentIdentity{DocumentIdentityLegacy, DocumentIdentityRootV1}}
	if err := request.Validate(); err != nil {
		test.Fatal(err)
	}
	for _, mutate := range []func(*ConversationFindPreparationRequest){
		func(value *ConversationFindPreparationRequest) { value.Version = 0 },
		func(value *ConversationFindPreparationRequest) { value.RepoID = "bad" },
		func(value *ConversationFindPreparationRequest) { value.Hash = "bad" },
		func(value *ConversationFindPreparationRequest) { value.Find.Query = "" },
		func(value *ConversationFindPreparationRequest) {
			value.Find.Query = strings.Repeat("한", MaxDocFindQueryRunes+1)
		},
		func(value *ConversationFindPreparationRequest) { value.Find.Query = "invalid\xff" },
		func(value *ConversationFindPreparationRequest) {
			value.Find.Cursor = strings.Repeat("x", MaxConversationFindPreparationBytes)
		},
		func(value *ConversationFindPreparationRequest) {
			value.DocumentIdentities = []DocumentIdentity{DocumentIdentityRootV1, DocumentIdentityRootV1}
		},
		func(value *ConversationFindPreparationRequest) {
			value.DocumentIdentities = []DocumentIdentity{"unknown"}
		},
	} {
		invalid := request
		mutate(&invalid)
		if invalid.Validate() == nil {
			test.Fatal("invalid preparation input accepted", invalid)
		}
	}
	request.Find.Query = strings.Repeat("🙂", MaxDocFindQueryRunes)
	if request.Validate() != nil {
		test.Fatal("valid UTF-8 query boundary rejected")
	}
}
