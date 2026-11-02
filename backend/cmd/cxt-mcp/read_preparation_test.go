package main

import "testing"

func TestReadPreparationOriginCompatibility(test *testing.T) {
	for _, scenario := range []struct {
		current, legacy, expected string
		invalid                   bool
	}{
		{},
		{current: "https://api.example.test", expected: "https://api.example.test"},
		{legacy: "https://api.example.test", expected: "https://api.example.test"},
		{current: "https://api.example.test", legacy: "https://api.example.test", expected: "https://api.example.test"},
		{current: "https://api.example.test", legacy: "https://different.example.test", invalid: true},
	} {
		origin, err := readPreparationAPIOrigin(scenario.current, scenario.legacy)
		if (err != nil) != scenario.invalid || origin != scenario.expected {
			test.Fatal("unexpected origin compatibility", origin, err)
		}
	}
	if memory, conversation, err := readPreparationRequesters("", ""); err != nil || memory != nil || conversation != nil {
		test.Fatal("empty configuration enabled forwarding", err)
	}
	if _, _, err := readPreparationRequesters("enabled", ""); err == nil {
		test.Fatal("prepared memory enabled without its remote preparation path")
	}
	memory, conversation, err := readPreparationRequesters("", "https://api.example.test")
	if err != nil || memory == nil || conversation == nil || any(memory) != any(conversation) {
		test.Fatal("read preparation did not share one bounded client", err)
	}
}
