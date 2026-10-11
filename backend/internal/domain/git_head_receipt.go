package domain

import "sort"

const MaxGitHeadSourceReads = 8

type GitHeadReceipt struct {
	ID               string                 `json:"id"`
	RepoID           ContentHash            `json:"repo_id"`
	GitOrigin        string                 `json:"git_origin"`
	Version          int64                  `json:"version,string"`
	Page             int                    `json:"page"`
	Provider         GitProviderObservation `json:"provider"`
	Heads            []GitHeadReceiptHead   `json:"heads"`
	More             bool                   `json:"more"`
	PriorSourceHeads []GitHeadReceiptHead   `json:"prior_source_heads,omitempty"`
}

type GitHeadReceiptHead struct {
	Ref           string `json:"ref"`
	Commit        string `json:"commit"`
	ObservationID string `json:"observation_id"`
}

func (receipt GitHeadReceipt) WithID() GitHeadReceipt {
	receipt.ID = ""
	receipt.ID = gitEvidenceKey(struct {
		EncodingVersion int `json:"encoding_version"`
		GitHeadReceipt
	}{EncodingVersion: 1, GitHeadReceipt: receipt})
	return receipt
}

func (receipt GitHeadReceipt) Validate() error {
	if receipt.ID != receipt.WithID().ID || ValidateContentHash(receipt.RepoID) != nil || !validPRContributionIdentity(receipt.GitOrigin) || receipt.Version <= 0 || receipt.Page < 1 || receipt.Provider.Validate() != nil || len(receipt.Heads) < 1 || len(receipt.Heads) > 100 {
		return ErrValidation
	}
	if len(receipt.PriorSourceHeads) > MaxGitHeadSourceReads {
		return ErrValidation
	}
	for _, heads := range [][]GitHeadReceiptHead{receipt.Heads, receipt.PriorSourceHeads} {
		for index, head := range heads {
			observation := GitRefObservation{RepoID: receipt.RepoID, GitOrigin: receipt.GitOrigin, Source: "reconciliation", Ref: head.Ref, After: head.Commit, Provider: &receipt.Provider}.WithID()
			if observation.Validate() != nil || head.ObservationID != observation.ID || (index > 0 && heads[index-1].Ref >= head.Ref) {
				return ErrValidation
			}
		}
	}
	return nil
}

func (receipt GitHeadReceipt) SourceHead(ref string) (GitHeadReceiptHead, bool) {
	for _, heads := range [][]GitHeadReceiptHead{receipt.Heads, receipt.PriorSourceHeads} {
		for _, head := range heads {
			if head.Ref == ref {
				return head, true
			}
		}
	}
	return GitHeadReceiptHead{}, false
}

func NewGitHeadReceiptWithSources(claim GitHeadScan, observations, sources []GitRefObservation, more bool) (*GitHeadReceipt, error) {
	if len(sources) > MaxGitHeadSourceReads {
		return nil, ErrValidation
	}
	receipt, err := NewGitHeadReceipt(claim, observations, more)
	if err != nil || len(sources) == 0 {
		return receipt, err
	}
	prior, err := NewGitHeadReceipt(claim, sources, false)
	if err != nil {
		return nil, err
	}
	if prior == nil || (len(observations) > 0 && receipt == nil) {
		return nil, ErrIntegrity
	}
	if receipt == nil {
		return nil, nil
	}
	if prior.Provider != receipt.Provider {
		return nil, ErrIntegrity
	}
	receipt.PriorSourceHeads = prior.Heads
	*receipt = receipt.WithID()
	return receipt, receipt.Validate()
}

func NewGitHeadReceipt(claim GitHeadScan, observations []GitRefObservation, more bool) (*GitHeadReceipt, error) {
	if claim.Validate() != nil || claim.State != "running" || claim.Version <= 0 || claim.LeaseUntil.IsZero() || len(observations) > 100 {
		return nil, ErrValidation
	}
	receipt := GitHeadReceipt{RepoID: claim.RepoID, GitOrigin: claim.GitOrigin, Version: claim.Version, Page: claim.Page, More: more}
	refs := map[string]bool{}
	qualified := len(observations) > 0 && observations[0].Provider != nil
	for _, observation := range observations {
		if err := observation.Validate(); err != nil {
			return nil, err
		}
		if observation.RepoID != claim.RepoID || observation.GitOrigin != claim.GitOrigin || observation.Source != "reconciliation" || refs[observation.Ref] || (observation.Provider != nil) != qualified {
			return nil, ErrIntegrity
		}
		refs[observation.Ref] = true
		if !qualified {
			continue
		}
		if receipt.Provider == (GitProviderObservation{}) {
			receipt.Provider = *observation.Provider
		}
		if receipt.Provider != *observation.Provider || observation.Before != "" || observation.After == "" || observation.Forced || observation.Delivery != "" {
			return nil, ErrIntegrity
		}
		receipt.Heads = append(receipt.Heads, GitHeadReceiptHead{Ref: observation.Ref, Commit: observation.After, ObservationID: observation.ID})
	}
	if !qualified {
		return nil, nil
	}
	sort.Slice(receipt.Heads, func(left, right int) bool { return receipt.Heads[left].Ref < receipt.Heads[right].Ref })
	receipt = receipt.WithID()
	if err := receipt.Validate(); err != nil {
		return nil, err
	}
	return &receipt, nil
}
