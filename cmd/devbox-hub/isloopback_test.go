package main

import "testing"

func TestIsLoopback(t *testing.T) {
	cases := []struct {
		addr string
		want bool
	}{
		{"127.0.0.1:8080", true},
		{"localhost:8080", true},
		{":8080", true},           // empty host = all interfaces, but treated as loopback-safe by default bind
		{"[::1]:8080", true},      // IPv6 loopback
		{"0.0.0.0:8080", false},   // all-interfaces = non-loopback
		{"192.168.1.1:8080", false},
		{"10.0.0.1:8080", false},
	}
	for _, c := range cases {
		if got := isLoopback(c.addr); got != c.want {
			t.Errorf("isLoopback(%q) = %v, want %v", c.addr, got, c.want)
		}
	}
}
