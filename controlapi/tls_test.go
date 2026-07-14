package controlapi

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cloudapp3/vmflow/config"
)

// issueCert creates a self-signed (parent == nil) or CA-signed certificate,
// writes cert + key PEM to dir, and returns the parsed cert + key for signing.
func issueCert(t *testing.T, dir, name string, isCA bool, parent *x509.Certificate, parentKey *ecdsa.PrivateKey) (*x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("gen key: %v", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		t.Fatalf("serial: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: name},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		IsCA:                  isCA,
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
	}
	if isCA {
		tmpl.KeyUsage |= x509.KeyUsageCertSign
	}
	signer, signerKey := tmpl, any(key)
	if parent != nil {
		signer, signerKey = parent, parentKey
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, signer, &key.PublicKey, signerKey)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse cert: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, name+".crt"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatalf("write cert: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, name+".key"), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	return cert, key
}

func TestBuildServerTLSConfig(t *testing.T) {
	dir := t.TempDir()
	caCert, caKey := issueCert(t, dir, "ca", true, nil, nil)
	issueCert(t, dir, "server", false, caCert, caKey)
	caBundle := filepath.Join(dir, "ca.crt")
	if err := os.WriteFile(caBundle, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caCert.Raw}), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Run("none returns nil", func(t *testing.T) {
		got, err := BuildServerTLSConfig(config.ControlTLSConfig{})
		if err != nil || got != nil {
			t.Fatalf("want nil,nil; got %+v,%v", got, err)
		}
	})
	t.Run("cert only rejected", func(t *testing.T) {
		if _, err := BuildServerTLSConfig(config.ControlTLSConfig{CertFile: dir + "/server.crt"}); err == nil {
			t.Fatal("want error for cert without key")
		}
	})
	t.Run("client ca without server key pair rejected", func(t *testing.T) {
		if _, err := BuildServerTLSConfig(config.ControlTLSConfig{ClientCAFile: caBundle}); err == nil {
			t.Fatal("want error for client CA without server cert and key")
		}
	})
	t.Run("both produces tls config with tls1.2", func(t *testing.T) {
		got, err := BuildServerTLSConfig(config.ControlTLSConfig{CertFile: dir + "/server.crt", KeyFile: dir + "/server.key"})
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if got == nil || len(got.Certificates) != 1 || got.MinVersion != tls.VersionTLS12 {
			t.Fatalf("unexpected: %+v", got)
		}
		if got.ClientAuth != tls.NoClientCert {
			t.Fatalf("expected no mTLS, got ClientAuth=%v", got.ClientAuth)
		}
	})
	t.Run("min version 1.3", func(t *testing.T) {
		got, err := BuildServerTLSConfig(config.ControlTLSConfig{CertFile: dir + "/server.crt", KeyFile: dir + "/server.key", MinVersion: "1.3"})
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if got.MinVersion != tls.VersionTLS13 {
			t.Fatalf("want tls1.3, got %x", got.MinVersion)
		}
	})
	t.Run("bad min version rejected", func(t *testing.T) {
		if _, err := BuildServerTLSConfig(config.ControlTLSConfig{CertFile: dir + "/server.crt", KeyFile: dir + "/server.key", MinVersion: "0.9"}); err == nil {
			t.Fatal("want error for bad min_version")
		}
	})
	t.Run("mtls when client ca set", func(t *testing.T) {
		got, err := BuildServerTLSConfig(config.ControlTLSConfig{CertFile: dir + "/server.crt", KeyFile: dir + "/server.key", ClientCAFile: caBundle})
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if got.ClientAuth != tls.RequireAndVerifyClientCert || got.ClientCAs == nil {
			t.Fatalf("expected mTLS, got ClientAuth=%v ClientCAs=%v", got.ClientAuth, got.ClientCAs)
		}
	})
	t.Run("missing cert file rejected", func(t *testing.T) {
		if _, err := BuildServerTLSConfig(config.ControlTLSConfig{CertFile: dir + "/missing.crt", KeyFile: dir + "/server.key"}); err == nil {
			t.Fatal("want error for missing cert file")
		}
	})
}

func TestBuildClientTLSConfig(t *testing.T) {
	dir := t.TempDir()
	caCert, caKey := issueCert(t, dir, "ca", true, nil, nil)
	issueCert(t, dir, "client", false, caCert, caKey)
	caBundle := filepath.Join(dir, "ca.crt")
	if err := os.WriteFile(caBundle, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caCert.Raw}), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Run("defaults", func(t *testing.T) {
		got, err := BuildClientTLSConfig(ClientTLSOptions{})
		if err != nil || got.InsecureSkipVerify || got.RootCAs != nil {
			t.Fatalf("unexpected: %+v %v", got, err)
		}
		if got.MinVersion != tls.VersionTLS12 {
			t.Fatalf("want tls1.2 min, got %x", got.MinVersion)
		}
	})
	t.Run("insecure", func(t *testing.T) {
		got, err := BuildClientTLSConfig(ClientTLSOptions{InsecureSkipVerify: true})
		if err != nil || !got.InsecureSkipVerify {
			t.Fatalf("want insecure skip verify; got %+v %v", got, err)
		}
	})
	t.Run("ca file", func(t *testing.T) {
		got, err := BuildClientTLSConfig(ClientTLSOptions{CAFile: caBundle})
		if err != nil || got.RootCAs == nil {
			t.Fatalf("want RootCAs set; got %+v %v", got, err)
		}
	})
	t.Run("client cert both", func(t *testing.T) {
		got, err := BuildClientTLSConfig(ClientTLSOptions{ClientCertFile: dir + "/client.crt", ClientKeyFile: dir + "/client.key"})
		if err != nil || len(got.Certificates) != 1 {
			t.Fatalf("want 1 client cert; got %+v %v", got, err)
		}
	})
	t.Run("client cert only rejected", func(t *testing.T) {
		if _, err := BuildClientTLSConfig(ClientTLSOptions{ClientCertFile: dir + "/client.crt"}); err == nil {
			t.Fatal("want error for cert without key")
		}
	})
}

func TestNewHTTPClient(t *testing.T) {
	t.Run("no opts returns default client", func(t *testing.T) {
		got, err := NewHTTPClient(ClientTLSOptions{}, 0)
		if err != nil || got != http.DefaultClient {
			t.Fatalf("want http.DefaultClient; got %v %v", got, err)
		}
	})
	t.Run("opts build transport with tls config", func(t *testing.T) {
		hc, err := NewHTTPClient(ClientTLSOptions{InsecureSkipVerify: true}, 5*time.Second)
		if err != nil || hc == nil || hc.Timeout != 5*time.Second {
			t.Fatalf("unexpected: %v %v", hc, err)
		}
		tr, ok := hc.Transport.(*http.Transport)
		if !ok || tr.TLSClientConfig == nil || !tr.TLSClientConfig.InsecureSkipVerify {
			t.Fatalf("want *http.Transport with InsecureSkipVerify TLSClientConfig, got %T", hc.Transport)
		}
	})
}
