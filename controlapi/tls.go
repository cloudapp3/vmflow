package controlapi

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/cloudapp3/vmflow/config"
)

// BuildServerTLSConfig builds a *tls.Config for the control API from cfg.
// It returns nil (plain HTTP) when neither cert nor key is configured. Setting
// ClientCAFile enables mutual TLS: every client must present a certificate
// signed by that CA.
func BuildServerTLSConfig(cfg config.ControlTLSConfig) (*tls.Config, error) {
	cfg.CertFile = strings.TrimSpace(cfg.CertFile)
	cfg.KeyFile = strings.TrimSpace(cfg.KeyFile)
	cfg.ClientCAFile = strings.TrimSpace(cfg.ClientCAFile)
	cfg.MinVersion = strings.TrimSpace(cfg.MinVersion)
	if cfg.CertFile == "" && cfg.KeyFile == "" {
		if cfg.ClientCAFile != "" {
			return nil, fmt.Errorf("control_tls: client_ca_file requires cert_file and key_file")
		}
		return nil, nil
	}
	if cfg.CertFile == "" || cfg.KeyFile == "" {
		return nil, fmt.Errorf("control_tls: cert_file and key_file must both be set")
	}
	cert, err := tls.LoadX509KeyPair(cfg.CertFile, cfg.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("control_tls: load key pair: %w", err)
	}
	minVer, err := parseTLSVersion(cfg.MinVersion)
	if err != nil {
		return nil, fmt.Errorf("control_tls: %w", err)
	}
	tlsCfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   minVer,
	}
	if cfg.ClientCAFile != "" {
		pem, err := os.ReadFile(cfg.ClientCAFile)
		if err != nil {
			return nil, fmt.Errorf("control_tls: read client_ca_file: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("control_tls: client_ca_file contains no valid certificates")
		}
		tlsCfg.ClientCAs = pool
		tlsCfg.ClientAuth = tls.RequireAndVerifyClientCert
	}
	return tlsCfg, nil
}

// ClientTLSOptions configures TLS for a control API client (vmflow ctl/tui).
type ClientTLSOptions struct {
	CAFile             string // CA bundle to verify the server certificate (for private/self-signed CAs)
	ClientCertFile     string // client certificate (required for mTLS when the server sets client_ca_file)
	ClientKeyFile      string // client key (required together with ClientCertFile)
	InsecureSkipVerify bool   // skip server certificate verification (debug only)
}

// Any reports whether any TLS option is set.
func (o ClientTLSOptions) Any() bool {
	return o.CAFile != "" || o.ClientCertFile != "" || o.ClientKeyFile != "" || o.InsecureSkipVerify
}

// BuildClientTLSConfig builds a *tls.Config for a control API client. MinVersion
// is pinned to TLS 1.2.
func BuildClientTLSConfig(o ClientTLSOptions) (*tls.Config, error) {
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if o.InsecureSkipVerify {
		cfg.InsecureSkipVerify = true
	}
	if o.CAFile != "" {
		pem, err := os.ReadFile(o.CAFile)
		if err != nil {
			return nil, fmt.Errorf("read ca file: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("ca file %q contains no valid certificates", o.CAFile)
		}
		cfg.RootCAs = pool
	}
	if o.ClientCertFile != "" || o.ClientKeyFile != "" {
		if o.ClientCertFile == "" || o.ClientKeyFile == "" {
			return nil, fmt.Errorf("client cert and key must both be set")
		}
		cert, err := tls.LoadX509KeyPair(o.ClientCertFile, o.ClientKeyFile)
		if err != nil {
			return nil, fmt.Errorf("load client key pair: %w", err)
		}
		cfg.Certificates = []tls.Certificate{cert}
	}
	return cfg, nil
}

// NewHTTPClient returns an *http.Client for talking to the control API. When no
// TLS option is set it returns http.DefaultClient (preserving existing
// behavior). A non-zero timeout applies an overall request deadline.
func NewHTTPClient(opts ClientTLSOptions, timeout time.Duration) (*http.Client, error) {
	if !opts.Any() {
		return http.DefaultClient, nil
	}
	tlsCfg, err := BuildClientTLSConfig(opts)
	if err != nil {
		return nil, err
	}
	return &http.Client{
		Timeout:   timeout,
		Transport: &http.Transport{TLSClientConfig: tlsCfg},
	}, nil
}

func parseTLSVersion(s string) (uint16, error) {
	switch s {
	case "", "1.2":
		return tls.VersionTLS12, nil
	case "1.3":
		return tls.VersionTLS13, nil
	default:
		return 0, fmt.Errorf("min_version must be \"1.2\" or \"1.3\", got %q", s)
	}
}
