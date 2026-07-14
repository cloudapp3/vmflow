package controlapi

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/cloudapp3/vmflow/config"
)

func TestEnsureSafeControlBinding(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))

	tests := []struct {
		name        string
		addr        string
		authEnabled bool
		allowRemote bool
		certFile    string
		keyFile     string
		clientCA    string
		wantErr     bool
	}{
		{name: "loopback no auth", addr: "127.0.0.1:19090"},
		{name: "ipv6 loopback no auth", addr: "[::1]:19090"},
		{name: "localhost no auth", addr: "localhost:19090"},
		{name: "all interfaces with auth", addr: "0.0.0.0:19090", authEnabled: true},
		{name: "all interfaces no auth rejected", addr: "0.0.0.0:19090", wantErr: true},
		{name: "ipv6 all interfaces no auth rejected", addr: "[::]:19090", wantErr: true},
		{name: "empty host no auth rejected", addr: ":19090", wantErr: true},
		{name: "non-loopback ip no auth rejected", addr: "10.0.0.5:19090", wantErr: true},
		{name: "hostname no auth rejected", addr: "myhost:19090", wantErr: true},
		{name: "all interfaces allowed via flag", addr: "0.0.0.0:19090", allowRemote: true},
		{name: "non-loopback allowed via flag", addr: "10.0.0.5:19090", allowRemote: true},
		{name: "malformed addr no auth rejected", addr: "not-a-host-port", wantErr: true},
		{name: "malformed addr with auth ok", addr: "not-a-host-port", authEnabled: true},
		{name: "non-loopback rejects incomplete mTLS", addr: "10.0.0.5:19090", clientCA: "ca.pem", wantErr: true},
		{name: "non-loopback allowed with mTLS", addr: "10.0.0.5:19090", certFile: "server.crt", keyFile: "server.key", clientCA: "ca.pem"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.File{
				ControlListenAddr: tt.addr,
				Auth:              config.AuthConfig{Enabled: tt.authEnabled},
				ControlTLS: config.ControlTLSConfig{
					CertFile:     tt.certFile,
					KeyFile:      tt.keyFile,
					ClientCAFile: tt.clientCA,
				},
			}
			err := EnsureSafeControlBinding(cfg, tt.allowRemote, logger)
			switch {
			case tt.wantErr && err == nil:
				t.Fatalf("expected error, got nil")
			case !tt.wantErr && err != nil:
				t.Fatalf("expected no error, got %v", err)
			}
		})
	}
}

func TestEnsureSafeControlBindingAllowRemoteWarns(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	cfg := config.File{ControlListenAddr: "0.0.0.0:19090"}

	if err := EnsureSafeControlBinding(cfg, true, logger); err != nil {
		t.Fatalf("expected no error with allowRemote, got %v", err)
	}
	if !strings.Contains(buf.String(), "explicitly allowed") {
		t.Fatalf("expected warn log about explicit allow, got: %s", buf.String())
	}
}
