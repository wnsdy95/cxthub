package domain

type PRJobAssociation struct {
	ProviderHistory         string `json:"provider_history"`
	ProviderCheckpointCount *int   `json:"provider_checkpoint_count,omitempty"`
	Participation           string `json:"participation"`
	ParticipationCount      *int   `json:"participation_count,omitempty"`
	PublicationCount        *int   `json:"publication_count,omitempty"`
	Contribution            string `json:"contribution"`
	ContributionCount       *int   `json:"contribution_count,omitempty"`
	CurrentApplicability    string `json:"current_applicability"`
}
