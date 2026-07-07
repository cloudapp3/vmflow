package tunnel

import (
	"io"
	"log/slog"
	"testing"

	"github.com/cloudapp3/vmflow/config"
)

func TestServerReloadConfigUpdatesACLAndDisconnectsInvalidSessions(t *testing.T) {
	server := NewServer(ServerConfig{
		Version: 1,
		TunnelServer: TunnelServerConfig{
			ListenAddr: "127.0.0.1:18080",
			Clients: []ServerClientACL{{
				ClientID: "home-01",
				Token:    "old-token",
				Allow:    AllowConfig{Protocols: []string{"tcp"}, RemotePorts: []int{2201}},
			}},
		},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	server.clients["home-01"] = &serverClientSession{
		clientID: "home-01",
		token:    "old-token",
		tunnels:  []TunnelSpec{{TunnelID: "ssh", Protocol: "tcp", RemoteListenPort: 2201, LocalAddr: "127.0.0.1", LocalPort: 22}},
		done:     make(chan struct{}),
	}

	result, err := server.ReloadConfig(ServerConfig{
		Version: 1,
		TunnelServer: TunnelServerConfig{
			ListenAddr: "127.0.0.1:18080",
			Clients: []ServerClientACL{{
				ClientID: "home-01",
				Token:    "new-token",
				Allow:    AllowConfig{Protocols: []string{"tcp"}, RemotePorts: []int{2201}},
			}},
		},
	})
	if err != nil {
		t.Fatalf("ReloadConfig() error = %v", err)
	}
	if !result.OK {
		t.Fatalf("ReloadConfig() OK = false: %+v", result)
	}
	if len(result.DisconnectedClients) != 1 || result.DisconnectedClients[0] != "home-01" {
		t.Fatalf("DisconnectedClients = %#v", result.DisconnectedClients)
	}
	if server.RunningClients() != 0 {
		t.Fatalf("RunningClients() = %d, want 0", server.RunningClients())
	}
	if _, ok := server.authenticate("home-01", "new-token"); !ok {
		t.Fatalf("new token was not accepted after reload")
	}
}

func TestServerPrecheckConfigWarnsForRestartOnlyFields(t *testing.T) {
	server := NewServer(ServerConfig{
		Version:           1,
		ControlListenAddr: "127.0.0.1:19091",
		TunnelServer: TunnelServerConfig{
			ListenAddr: "127.0.0.1:18080",
			Clients:    []ServerClientACL{{ClientID: "home-01", Token: "secret"}},
		},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	check := server.PrecheckConfig(ServerConfig{
		Version:           1,
		ControlListenAddr: "127.0.0.1:19092",
		TunnelServer: TunnelServerConfig{
			ListenAddr: "127.0.0.1:18081",
			Clients:    []ServerClientACL{{ClientID: "home-01", Token: "secret"}},
		},
	})
	if !check.OK {
		t.Fatalf("PrecheckConfig() OK = false: %+v", check)
	}
	if check.WarningCount < 2 {
		t.Fatalf("WarningCount = %d, want at least 2", check.WarningCount)
	}
}

func TestTunnelControlReloadRequiresAdmin(t *testing.T) {
	path := writeTempConfig(t, []byte(`version: 1
control_listen_addr: 127.0.0.1:19091
auth:
  enabled: true
  tokens:
    - name: viewer
      token: view-token
      role: viewer
    - name: admin
      token: admin-token
      role: admin
tunnel_server:
  listen_addr: 127.0.0.1:18080
  clients:
    - client_id: home-01
      token: secret
`))
	cfg, err := LoadServerConfig(path)
	if err != nil {
		t.Fatalf("LoadServerConfig() error = %v", err)
	}
	server := NewServer(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	handler := NewControlHandler(server, ControlHandlerOptions{ConfigPath: path, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})

	rec := performControlRequest(handler, "POST", "/v1/tunnel/reload", "Bearer view-token")
	if rec.Code != 403 {
		t.Fatalf("viewer reload status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
	rec = performControlRequest(handler, "POST", "/v1/tunnel/reload", "Bearer admin-token")
	if rec.Code != 200 {
		t.Fatalf("control reload status = %d, body=%s", rec.Code, rec.Body.String())
	}
}

func TestServerReloadConfigKeepsRuntimeListenAddrs(t *testing.T) {
	server := NewServer(ServerConfig{
		Version:           1,
		ControlListenAddr: "127.0.0.1:19091",
		Auth:              config.AuthConfig{},
		TunnelServer: TunnelServerConfig{
			ListenAddr: "127.0.0.1:18080",
			Clients:    []ServerClientACL{{ClientID: "home-01", Token: "secret"}},
		},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	_, err := server.ReloadConfig(ServerConfig{
		Version:           1,
		ControlListenAddr: "127.0.0.1:19092",
		TunnelServer: TunnelServerConfig{
			ListenAddr: "127.0.0.1:18081",
			Clients:    []ServerClientACL{{ClientID: "home-01", Token: "secret"}},
		},
	})
	if err != nil {
		t.Fatalf("ReloadConfig() error = %v", err)
	}
	cfg := server.Config()
	if cfg.ControlListenAddr != "127.0.0.1:19091" {
		t.Fatalf("ControlListenAddr = %q", cfg.ControlListenAddr)
	}
	if cfg.TunnelServer.ListenAddr != "127.0.0.1:18080" {
		t.Fatalf("TunnelServer.ListenAddr = %q", cfg.TunnelServer.ListenAddr)
	}
}
