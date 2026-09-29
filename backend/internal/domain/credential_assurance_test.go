package domain

import (
	"testing"
	"time"
)

func TestAssuranceDeadlineIsAuthenticationBound(t *testing.T) {
	now := time.Now().UTC()
	auth := now.Add(-time.Minute)
	p := DefaultAssurancePolicy(NewID("ep_"))
	browser := now.Add(24 * time.Hour)
	for _, tc := range []struct {
		name    string
		browser time.Time
		idp     *time.Time
		want    time.Time
	}{
		{"policy", browser, nil, auth.Add(8 * time.Hour)},
		{"browser", now.Add(time.Hour), nil, now.Add(time.Hour)},
		{"provider", browser, ptrAssuranceTime(now.Add(30 * time.Minute)), now.Add(30 * time.Minute)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := p.AssuranceDeadline(auth, tc.browser, now, tc.idp)
			if err != nil || !got.Equal(tc.want) {
				t.Fatal(got, err)
			}
		})
	}
	for _, auth := range []time.Time{time.Time{}, now.Add(-11 * time.Minute), now.Add(2 * time.Minute)} {
		if _, err := p.AssuranceDeadline(auth, browser, now, nil); err == nil {
			t.Fatal("invalid auth accepted", auth)
		}
	}
	for _, hours := range []int{0, 25} {
		p.MaxAgeHours = hours
		if _, err := p.AssuranceDeadline(auth, browser, now, nil); err == nil {
			t.Fatal("invalid policy accepted")
		}
	}
}
func ptrAssuranceTime(t time.Time) *time.Time { return &t }
