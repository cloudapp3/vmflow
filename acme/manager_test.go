package acme

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testCertificate(t *testing.T, domains ...string) (*tls.Certificate, []byte, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: domains[0]},
		DNSNames:     domains,
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse certificate: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, certPEM, keyPEM
}

func TestManagerSaveLoadAndPreloadAliases(t *testing.T) {
	dir := t.TempDir()
	cert, _, _ := testCertificate(t, "example.com", "api.example.com")
	manager := NewManager(dir, nil)
	manager.certCache["example.com"] = cert
	manager.certCache["api.example.com"] = cert
	manager.cacheKeys["example.com"] = "example.com"
	manager.cacheKeys["api.example.com"] = "example.com"
	if err := manager.saveToDisk("example.com", cert); err != nil {
		t.Fatalf("saveToDisk: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "example.com") {
			t.Fatalf("domain leaked into cache filename %q", entry.Name())
		}
	}

	reloaded := NewManager(dir, nil)
	reloaded.Preload(context.Background())
	for _, domain := range []string{"example.com", "api.example.com"} {
		got, err := reloaded.GetCertificate(&tls.ClientHelloInfo{ServerName: domain})
		if err != nil {
			t.Fatalf("GetCertificate(%s): %v", domain, err)
		}
		if got.Leaf == nil || got.Leaf.SerialNumber.Cmp(cert.Leaf.SerialNumber) != 0 {
			t.Fatalf("GetCertificate(%s) returned the wrong certificate", domain)
		}
	}
}

func TestManagerRejectsUnsafeCertificateDomains(t *testing.T) {
	dir := t.TempDir()
	manager := NewManager(dir, nil)
	cert, _, _ := testCertificate(t, "example.com")
	outside := filepath.Join(filepath.Dir(dir), "outside-acme-sentinel")
	if err := os.WriteFile(outside, []byte("unchanged"), 0o600); err != nil {
		t.Fatalf("write sentinel: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(outside) })

	for _, domain := range []string{"../../outside-acme-sentinel", `..\..\outside-acme-sentinel`, "/tmp/outside-acme-sentinel"} {
		if err := manager.saveToDisk(domain, cert); err == nil {
			t.Errorf("saveToDisk(%q) unexpectedly succeeded", domain)
		}
		if _, err := manager.GetCertificate(&tls.ClientHelloInfo{ServerName: domain}); err == nil {
			t.Errorf("GetCertificate(%q) unexpectedly succeeded", domain)
		}
		if err := manager.Obtain(context.Background(), []string{domain}); err == nil {
			t.Errorf("Obtain(%q) unexpectedly succeeded", domain)
		}
	}
	data, err := os.ReadFile(outside)
	if err != nil {
		t.Fatalf("read sentinel: %v", err)
	}
	if string(data) != "unchanged" {
		t.Fatalf("outside sentinel changed to %q", data)
	}
}

func TestManagerPreloadMigratesLegacyCache(t *testing.T) {
	dir := t.TempDir()
	_, certPEM, keyPEM := testCertificate(t, "legacy.example.com")
	legacyCert := filepath.Join(dir, "legacy.example.com.crt")
	legacyKey := filepath.Join(dir, "legacy.example.com.key")
	if err := os.WriteFile(legacyCert, certPEM, 0o644); err != nil {
		t.Fatalf("write legacy cert: %v", err)
	}
	if err := os.WriteFile(legacyKey, keyPEM, 0o600); err != nil {
		t.Fatalf("write legacy key: %v", err)
	}

	manager := NewManager(dir, nil)
	manager.Preload(context.Background())
	if _, err := manager.GetCertificate(&tls.ClientHelloInfo{ServerName: "legacy.example.com"}); err != nil {
		t.Fatalf("GetCertificate after preload: %v", err)
	}
	if _, err := os.Lstat(legacyCert); !os.IsNotExist(err) {
		t.Fatalf("legacy cert still exists: %v", err)
	}
	if _, err := os.Lstat(legacyKey); !os.IsNotExist(err) {
		t.Fatalf("legacy key still exists: %v", err)
	}
}
