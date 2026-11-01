package main

import (
	"testing"
)

func TestMemoryPreparationRequesterConfiguration(test *testing.T) {
	if requester, err := memoryPreparationRequester("", ""); err != nil || requester != nil {
		test.Fatal("forwarding enabled by default", err)
	}
	if _, err := memoryPreparationRequester("enabled", ""); err == nil {
		test.Fatal("enabled prepared reads lack a preparation path")
	}
	for _, origin := range []string{"https://api.example.test", "http://127.0.0.1:8907"} {
		if requester, err := memoryPreparationRequester("enabled", origin); err != nil || requester == nil {
			test.Fatal("valid delegated API rejected", err)
		}
	}
	for _, origin := range []string{"http://api.example.test", "https://api.example.test/route", "https://user:secret@api.example.test", "https://api.example.test?token=secret"} {
		if _, err := memoryPreparationRequester("enabled", origin); err == nil {
			test.Fatal("invalid delegated API accepted")
		}
	}
}
