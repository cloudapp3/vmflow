package tunnel

import "testing"

func TestLoadClientConfigDefaultsAndValidation(t *testing.T) {
	raw := []byte(`version: 1
log:
  level: error
  format: text
tunnel_client:
  server_addr: 127.0.0.1:18080
  client_id: home-01
  token: secret
  tunnels:
    - tunnel_id: ssh
      remote_listen_port: 2201
      local_port: 22
`)
	path := writeTempConfig(t, raw)
	cfg, err := LoadClientConfig(path)
	if err != nil {
		t.Fatalf("LoadClientConfig() error = %v", err)
	}
	if cfg.TunnelClient.DialTimeout != DefaultDialTimeout {
		t.Fatalf("DialTimeout = %q, want %q", cfg.TunnelClient.DialTimeout, DefaultDialTimeout)
	}
	if got := cfg.TunnelClient.Tunnels[0].Protocol; got != "tcp" {
		t.Fatalf("Protocol = %q, want tcp", got)
	}
	if got := cfg.TunnelClient.Tunnels[0].RemoteListenAddr; got != "0.0.0.0" {
		t.Fatalf("RemoteListenAddr = %q, want 0.0.0.0", got)
	}
	if got := cfg.TunnelClient.Tunnels[0].LocalAddr; got != "127.0.0.1" {
		t.Fatalf("LocalAddr = %q, want 127.0.0.1", got)
	}
}

func TestLoadServerConfigRejectsDuplicateClient(t *testing.T) {
	raw := []byte(`version: 1
tunnel_server:
  listen_addr: 127.0.0.1:18080
  clients:
    - client_id: home-01
      token: secret-a
    - client_id: home-01
      token: secret-b
`)
	path := writeTempConfig(t, raw)
	_, err := LoadServerConfig(path)
	if err == nil {
		t.Fatalf("LoadServerConfig() expected duplicate client error")
	}
}

func TestACLAllowsProtocolAndRemotePort(t *testing.T) {
	allow := AllowConfig{Protocols: []string{"tcp"}, RemotePorts: []int{2201}}
	if err := aclAllows(allow, TunnelSpec{Protocol: "tcp", RemoteListenPort: 2201}); err != nil {
		t.Fatalf("aclAllows() unexpected error: %v", err)
	}
	if err := aclAllows(allow, TunnelSpec{Protocol: "udp", RemoteListenPort: 2201}); err == nil {
		t.Fatalf("aclAllows() expected protocol error")
	}
	if err := aclAllows(allow, TunnelSpec{Protocol: "tcp", RemoteListenPort: 2202}); err == nil {
		t.Fatalf("aclAllows() expected port error")
	}
}
