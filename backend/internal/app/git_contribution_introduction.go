package app

import (
	"context"
	"errors"
	"fmt"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

var errGitContributionEvidencePending = errors.New("generic Git contribution requires complete introduction evidence")
var errGitContributionEvidenceLimit = fmt.Errorf("%w: traversal budget exceeded", errGitContributionEvidencePending)
var errGitContributionAlreadyPresent = errors.New("source code was already present at the destination baseline")
var errGitContributionRangeDiscontinuous = fmt.Errorf("%w: destination baseline is not on the first-parent path", errGitContributionEvidencePending)

type gitIntroductionReach uint8

const (
	gitIntroductionAbsent gitIntroductionReach = iota
	gitIntroductionPresent
	gitIntroductionUnknown
)

func locateGitContributionIntroduction(ctx context.Context, evidence *codeEvidence, source, destination string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if evidence == nil || domain.ValidateGitOID(source) != nil || domain.ValidateGitOID(destination) != nil || len(source) != len(destination) {
		return "", domain.ErrValidation
	}
	read := func(commit string) (domain.GitCommitTree, error) {
		proof, err := evidence.GetGitCommitTree(ctx, evidence.repo, evidence.origin, commit)
		if err == nil && (proof.Commit != commit || proof.Validate() != nil) {
			return domain.GitCommitTree{}, domain.ErrIntegrity
		}
		return proof, err
	}
	if _, err := read(source); err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			if evidence.limited {
				return "", errGitContributionEvidenceLimit
			}
			return "", errGitContributionEvidencePending
		}
		return "", err
	}
	memo := map[string]gitIntroductionReach{}
	active := map[string]bool{}
	visited := 0
	truncated := false
	var contains func(string) (gitIntroductionReach, error)
	contains = func(commit string) (gitIntroductionReach, error) {
		if err := ctx.Err(); err != nil {
			return gitIntroductionUnknown, err
		}
		if reach, exists := memo[commit]; exists {
			return reach, nil
		}
		if active[commit] {
			return gitIntroductionUnknown, domain.ErrIntegrity
		}
		if visited >= maxCodeEvidenceReads {
			truncated = true
			return gitIntroductionUnknown, nil
		}
		visited++
		proof, err := read(commit)
		if errors.Is(err, domain.ErrNotFound) {
			memo[commit] = gitIntroductionUnknown
			return gitIntroductionUnknown, nil
		}
		if err != nil {
			return gitIntroductionUnknown, err
		}
		active[commit] = true
		defer delete(active, commit)
		reach := gitIntroductionAbsent
		if commit == source {
			reach = gitIntroductionPresent
		}
		for _, parent := range proof.Parents {
			parentReach, err := contains(parent)
			if err != nil {
				return gitIntroductionUnknown, err
			}
			if parentReach == gitIntroductionPresent || (parentReach == gitIntroductionUnknown && reach == gitIntroductionAbsent) {
				reach = parentReach
			}
		}
		memo[commit] = reach
		return reach, nil
	}
	reach, err := contains(destination)
	if err != nil {
		return "", err
	}
	if reach == gitIntroductionAbsent {
		return "", domain.ErrNotFound
	}
	if reach == gitIntroductionUnknown {
		if truncated || evidence.limited {
			return "", errGitContributionEvidenceLimit
		}
		return "", errGitContributionEvidencePending
	}
	for current, steps := destination, 0; steps < maxCodeEvidenceReads; steps++ {
		if current == source {
			return current, nil
		}
		proof, err := read(current)
		if err != nil {
			return "", err
		}
		if len(proof.Parents) == 0 {
			return "", domain.ErrIntegrity
		}
		parent := proof.Parents[0]
		parentReach, known := memo[parent]
		if !known {
			if truncated || evidence.limited {
				return "", errGitContributionEvidenceLimit
			}
			return "", errGitContributionEvidencePending
		}
		switch parentReach {
		case gitIntroductionAbsent:
			return current, nil
		case gitIntroductionUnknown:
			if truncated || evidence.limited {
				return "", errGitContributionEvidenceLimit
			}
			return "", errGitContributionEvidencePending
		case gitIntroductionPresent:
			current = parent
		}
	}
	return "", errGitContributionEvidenceLimit
}

func locateGitContributionInRange(ctx context.Context, evidence *codeEvidence, source, before, after string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if evidence == nil || domain.ValidateGitOID(source) != nil || domain.ValidateGitOID(before) != nil || domain.ValidateGitOID(after) != nil || len(source) != len(before) || len(before) != len(after) {
		return "", domain.ErrValidation
	}
	if _, err := locateGitContributionIntroduction(ctx, evidence, source, before); err == nil {
		return "", errGitContributionAlreadyPresent
	} else if !errors.Is(err, domain.ErrNotFound) {
		return "", err
	}
	introduction, err := locateGitContributionIntroduction(ctx, evidence, source, after)
	if err != nil {
		return "", err
	}
	seen := map[string]bool{}
	introduced := false
	for current, steps := after, 0; steps < maxCodeEvidenceReads; steps++ {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if current == before {
			if introduced {
				return introduction, nil
			}
			return "", domain.ErrIntegrity
		}
		if seen[current] {
			return "", domain.ErrIntegrity
		}
		seen[current] = true
		introduced = introduced || current == introduction
		proof, err := evidence.GetGitCommitTree(ctx, evidence.repo, evidence.origin, current)
		if errors.Is(err, domain.ErrNotFound) {
			if evidence.limited {
				return "", errGitContributionEvidenceLimit
			}
			return "", errGitContributionEvidencePending
		}
		if err != nil {
			return "", err
		}
		if proof.Commit != current || proof.Validate() != nil {
			return "", domain.ErrIntegrity
		}
		if len(proof.Parents) == 0 {
			return "", errGitContributionRangeDiscontinuous
		}
		current = proof.Parents[0]
	}
	return "", errGitContributionEvidenceLimit
}
