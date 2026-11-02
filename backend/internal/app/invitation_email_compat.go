package app

import "github.com/wnsdy95/cxthub/backend/internal/ports/outbound"

func (service *IdentityService) invitationAuthority() invitationAuthority {
	enterprises, _ := service.repositories.(outbound.EnterpriseStore)
	return invitationAuthority{organizations: service.organization, enterprises: enterprises}
}

func (service *IdentityService) InvitationEmailWorkerConfig(profile InvitationEmailProfile) InvitationEmailWorkerConfig {
	queue, _ := service.repositories.(outbound.InvitationEmailStore)
	invitations, _ := service.repositories.(outbound.CollaborationInviteStore)
	enterprises, _ := service.repositories.(outbound.EnterpriseStore)
	transactions, _ := service.repositories.(outbound.IdentityTransactions)
	config := InvitationEmailWorkerConfig{Profile: profile, Queue: queue, Invitations: invitations,
		Organizations: service.organization, Enterprises: enterprises, Transactions: transactions}
	if service.invitationEmail != nil {
		config.Sender = service.invitationEmail.sender
	}
	return config
}

func (service *IdentityService) invitationEmailWorker() *InvitationEmailWorker {
	return assembleInvitationEmailWorker(service.InvitationEmailWorkerConfig(InvitationEmailCompatibility))
}
