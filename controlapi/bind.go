package controlapi

import (
	"fmt"
	"log/slog"
	"net"
	"strings"

	"github.com/cloudapp3/vmflow/config"
)

// EnsureSafeControlBinding enforces that the control API does not start on a
// non-loopback address without authentication.
//
// It returns nil when the binding is safe to start, and a descriptive error
// otherwise. A binding is considered safe when authentication is enabled (any
// address), mutual TLS is configured (client_ca_file, any address), or the
// control API listens on a loopback address.
//
// A non-loopback binding without auth or mTLS is rejected unless allowRemote is
// true, in which case a warning is logged and nil is returned. The caller MUST
// abort startup (e.g. os.Exit) when this returns a non-nil error.
func EnsureSafeControlBinding(cfg config.File, allowRemote bool, logger *slog.Logger) error {
	if cfg.Auth.Enabled || hasMutualTLSConfig(cfg.ControlTLS) {
		return nil
	}
	host, _, err := net.SplitHostPort(cfg.ControlListenAddr)
	if err != nil {
		return fmt.Errorf(
			"auth is disabled and control_listen_addr %q is not a valid host:port "+
				"(enable auth, bind to 127.0.0.1, or pass --insecure-allow-remote-control)",
			cfg.ControlListenAddr,
		)
	}
	if isLoopbackHost(host) {
		return nil
	}
	if allowRemote {
		if logger != nil {
			logger.Warn("control api exposed without auth (explicitly allowed)",
				"component", "daemon", "event", "auth_disabled_exposed_allowed",
				"control_listen_addr", cfg.ControlListenAddr)
		}
		return nil
	}
	return fmt.Errorf(
		"control API is bound to %q without authentication, which would allow "+
			"unauthenticated remote control. Bind to 127.0.0.1, enable auth "+
			"(auth.enabled + tokens), or pass --insecure-allow-remote-control to acknowledge",
		cfg.ControlListenAddr,
	)
}

func hasMutualTLSConfig(cfg config.ControlTLSConfig) bool {
	return strings.TrimSpace(cfg.CertFile) != "" &&
		strings.TrimSpace(cfg.KeyFile) != "" &&
		strings.TrimSpace(cfg.ClientCAFile) != ""
}

// isLoopbackHost reports whether host is a loopback address. An empty host
// (":port") binds all interfaces and is therefore treated as non-loopback.
// Hostnames other than "localhost" are treated as non-loopback because they may
// resolve to any address.
func isLoopbackHost(host string) bool {
	host = strings.TrimSpace(host)
	if host == "" {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}
