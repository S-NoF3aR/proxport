package main

import (
	"strings"
	"testing"
)

func TestNormalizeHost(t *testing.T) {
	tests := map[string]string{
		"App.Example.COM":              "app.example.com",
		"app.example.com:443":          "app.example.com",
		"https://app.example.com/path": "app.example.com",
		"[2001:db8::1]:443":            "2001:db8::1",
	}

	for input, expected := range tests {
		if got := normalizeHost(input); got != expected {
			t.Fatalf("normalizeHost(%q) = %q, want %q", input, got, expected)
		}
	}
}

func TestParseHTTPHost(t *testing.T) {
	header := []byte("GET / HTTP/1.1\r\nHost: App.Example.COM:443\r\nUser-Agent: test\r\n\r\n")

	if got := parseHTTPHost(header); got != "app.example.com" {
		t.Fatalf("parseHTTPHost() = %q, want %q", got, "app.example.com")
	}
}

func TestValidateConfigRejectsHostFilterOnUDP(t *testing.T) {
	cfg := Config{
		ListenAddress: "0.0.0.0",
		Forwards: []ForwardRule{
			{
				Name:       "bad-udp",
				Protocol:   "udp",
				ListenPort: 27015,
				TargetHost: "192.168.100.103",
				TargetPort: 27015,
				Host:       "game.example.com",
			},
		},
	}

	err := validateConfig(&cfg)
	if err == nil || !strings.Contains(err.Error(), "host filtering is only supported for tcp rules") {
		t.Fatalf("validateConfig() error = %v, want host filter UDP rejection", err)
	}
}
