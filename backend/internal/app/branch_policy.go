package app

type branchContributionFacts []contextContributionProjection

func (facts branchContributionFacts) Len() int {
	return len(facts)
}
