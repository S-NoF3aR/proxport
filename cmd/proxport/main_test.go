package main

import (
	"errors"
	"net"
	"testing"
)

func TestParseYAMLConfig_AllowsInlineComments(t *testing.T) {
	t.Parallel()

	configText := `
listen_address: 0.0.0.0 # bind all interfaces
dial_timeout: 5s # upstream timeout
forwards:
  - name: ssh-vm-101 # ssh endpoint
    protocol: tcp
    listen_port: 2222 # public port
    target_host: 192.168.100.101
    target_port: 22
`

	cfg, err := parseYAMLConfig([]byte(configText))
	if err != nil {
		t.Fatalf("parseYAMLConfig returned error: %v", err)
	}

	if cfg.ListenAddress != "0.0.0.0" {
		t.Fatalf("unexpected listen address: %q", cfg.ListenAddress)
	}
	if cfg.DialTimeout.Duration.String() != "5s" {
		t.Fatalf("unexpected dial timeout: %v", cfg.DialTimeout.Duration)
	}
	if len(cfg.Forwards) != 1 {
		t.Fatalf("expected 1 forward rule, got %d", len(cfg.Forwards))
	}
	if cfg.Forwards[0].ListenPort != 2222 {
		t.Fatalf("unexpected listen_port: %d", cfg.Forwards[0].ListenPort)
	}
}

func TestStripYAMLInlineComment_PreservesHashInQuotes(t *testing.T) {
	t.Parallel()

	got := stripYAMLInlineComment(`"value#inside" # trailing comment`)
	if got != `"value#inside"` {
		t.Fatalf("unexpected value after stripping comments: %q", got)
	}
}

func TestIsClosedNetworkError(t *testing.T) {
	t.Parallel()

	if !isClosedNetworkError(net.ErrClosed) {
		t.Fatal("expected net.ErrClosed to be recognized as closed network error")
	}

	wrapped := errors.New("read failed: use of closed network connection")
	if !isClosedNetworkError(wrapped) {
		t.Fatal("expected legacy closed-network error message to be recognized")
	}

	if isClosedNetworkError(nil) {
		t.Fatal("nil error should not be treated as closed network error")
	}
}
