package domain

import (
	"errors"
	"testing"
	"time"
)

func TestUserPreferencesPatchPreservesIdentityAndUnselectedFields(test *testing.T) {
	user := User{ID: "preferences-user", Email: "owner@example.test", Name: "Provider Name", Username: "owner", Nickname: "Before", LoadMode: "full", Avatar: "data:image/png;base64,aA==", Locale: "en", CreatedAt: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)}
	nickname := "  After  "
	updated, err := ApplyUserPreferences(user, UserPreferencesPatch{Nickname: &nickname})
	expected := user
	expected.Nickname = "After"
	if err != nil || updated != expected || user.Nickname != "Before" {
		test.Fatal("partial patch changed authoritative identity or unselected preferences", updated, err)
	}
	empty := ""
	cleared, err := ApplyUserPreferences(updated, UserPreferencesPatch{Nickname: &empty, Avatar: &empty, LoadMode: &empty, Locale: &empty})
	expected.Nickname, expected.Avatar, expected.LoadMode, expected.Locale = "", "", "", ""
	if err != nil || cleared != expected {
		test.Fatal("explicit empty preferences did not clear only the selected fields", cleared, err)
	}
	if unchanged, err := ApplyUserPreferences(user, UserPreferencesPatch{}); err != nil || unchanged != user {
		test.Fatal("empty patch changed the user", unchanged, err)
	}
}

func TestUserPreferencesPatchRejectsInvalidInputWithoutPartialResult(test *testing.T) {
	user := User{ID: "preferences-user", Username: "owner", Nickname: "Before"}
	invalidMode, invalidLocale, invalidAvatar := "summary", "de", "https://external.example.test/avatar.png"
	for _, patch := range []UserPreferencesPatch{{LoadMode: &invalidMode}, {Locale: &invalidLocale}, {Avatar: &invalidAvatar}} {
		if got, err := ApplyUserPreferences(user, patch); got != (User{}) || !errors.Is(err, ErrValidation) {
			test.Fatal("invalid preferences returned a partial account", got, err)
		}
	}
	if user.Nickname != "Before" {
		test.Fatal("validation mutated the input user")
	}
}
