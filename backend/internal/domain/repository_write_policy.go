package domain

func CanWriteRepositoryContext(repository Repository, role MemberRole, exists bool, action string) bool {
	if !exists || repository.Archived || !role.AtLeast(RoleMember) {
		return false
	}
	switch action {
	case "":
		return true
	case "manage":
		return role.AtLeast(RoleMaintainer)
	case "settings":
		return role.AtLeast(RoleMaintainer) && PolicyAllows(repository.SettingsPolicy, role == RoleOwner)
	default:
		return false
	}
}
