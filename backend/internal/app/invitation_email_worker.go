package app

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type InvitationEmailProfile string

const (
	InvitationEmailCompatibility InvitationEmailProfile = "compatibility"
	InvitationEmailComplete      InvitationEmailProfile = "complete"
)

type InvitationEmailWorkerConfig struct {
	Profile       InvitationEmailProfile
	Queue         outbound.InvitationEmailStore
	Invitations   InvitationReader
	Organizations InvitationOrganizationReader
	Enterprises   InvitationEnterpriseReader
	Transactions  outbound.IdentityTransactions
	Sender        outbound.InvitationMailer
}

type InvitationEmailWorker struct {
	queue        outbound.InvitationEmailStore
	invitations  InvitationReader
	authority    invitationAuthority
	transactions outbound.IdentityTransactions
	sender       outbound.InvitationMailer
}

func NewInvitationEmailWorker(config InvitationEmailWorkerConfig) (*InvitationEmailWorker, error) {
	if config.Profile != InvitationEmailCompatibility && config.Profile != InvitationEmailComplete {
		return nil, domain.ErrValidation
	}
	for _, dependency := range []any{config.Queue, config.Invitations, config.Organizations, config.Enterprises, config.Transactions, config.Sender} {
		if dependency != nil && !rootAdapterPresent(dependency) {
			return nil, domain.ErrValidation
		}
	}
	if config.Sender != nil {
		if !rootAdapterPresent(config.Queue) {
			return nil, domain.ErrCatalogUnsupported
		}
		if config.Profile == InvitationEmailComplete {
			for _, dependency := range []any{config.Invitations, config.Organizations, config.Enterprises, config.Transactions} {
				if !rootAdapterPresent(dependency) {
					return nil, domain.ErrCatalogUnsupported
				}
			}
		}
	}
	return assembleInvitationEmailWorker(config), nil
}

func assembleInvitationEmailWorker(config InvitationEmailWorkerConfig) *InvitationEmailWorker {
	return &InvitationEmailWorker{queue: config.Queue, invitations: config.Invitations,
		authority:    invitationAuthority{organizations: config.Organizations, enterprises: config.Enterprises},
		transactions: config.Transactions, sender: config.Sender}
}

func (worker *InvitationEmailWorker) Run(ctx context.Context) {
	worker.RunInvitationEmailWorker(ctx)
}
