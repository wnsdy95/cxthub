package domain

import "strings"

type UserPreferencesPatch struct {
	Nickname *string
	LoadMode *string
	Avatar   *string
	Locale   *string
}

func ApplyUserPreferences(user User, patch UserPreferencesPatch) (User, error) {
	if patch.Nickname != nil {
		user.Nickname = strings.TrimSpace(*patch.Nickname)
	}
	if patch.Avatar != nil {
		user.Avatar = strings.TrimSpace(*patch.Avatar)
		if err := ValidateAvatarDataURL(user.Avatar); err != nil {
			return User{}, err
		}
	}
	if patch.LoadMode != nil {
		loadMode := strings.TrimSpace(*patch.LoadMode)
		if loadMode != "" && loadMode != "full" && loadMode != "reconstructed" && loadMode != "memory" {
			return User{}, ErrValidation
		}
		user.LoadMode = loadMode
	}
	if patch.Locale != nil {
		locale := strings.TrimSpace(*patch.Locale)
		if locale != "" && locale != "ko" && locale != "en" {
			return User{}, ErrValidation
		}
		user.Locale = locale
	}
	if err := ValidateUserRecord(user); err != nil {
		return User{}, err
	}
	return user, nil
}
