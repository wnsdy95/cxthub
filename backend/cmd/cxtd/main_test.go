package main

import "testing"

func TestPostgresRequiredForEveryExternalBind(t *testing.T) {
	tests := []struct {
		addr       string
		configured string
		want       bool
	}{
		{addr: "127.0.0.1:8907", want: false},
		{addr: "localhost:8907", want: false},
		{addr: "[::1]:8907", want: false},
		{addr: "127.0.0.1:8907", configured: "1", want: true},
		{addr: "0.0.0.0:8907", want: true},
		{addr: ":8907", want: true},
		{addr: "10.0.0.5:8907", want: true},
	}
	for _, test := range tests {
		t.Run(test.addr+"/"+test.configured, func(t *testing.T) {
			if got := postgresRequired(test.addr, test.configured); got != test.want {
				t.Fatalf("postgresRequired(%q, %q) = %v, want %v", test.addr, test.configured, got, test.want)
			}
		})
	}
}
