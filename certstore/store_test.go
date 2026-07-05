package certstore

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
	"testing"
	"time"
)

// generateSelfSignedCert creates a self-signed certificate for testing.
func generateSelfSignedCert(t *testing.T, domain string) (certPEM, keyPEM []byte) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{
			CommonName: domain,
		},
		DNSNames:    []string{domain},
		NotBefore:   time.Now().Add(-1 * time.Hour),
		NotAfter:    time.Now().Add(90 * 24 * time.Hour),
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}

	certDER, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}

	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})

	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	return certPEM, keyPEM
}

func TestStoreGetCertificateEmpty(t *testing.T) {
	s := New(nil, "")
	hello := &tls.ClientHelloInfo{ServerName: "example.com"}
	_, err := s.GetCertificate(hello)
	if err == nil {
		t.Fatal("expected error for empty store")
	}
}

func TestStoreGetCertificateNil(t *testing.T) {
	s := New(nil, "")
	_, err := s.GetCertificate(nil)
	if err == nil {
		t.Fatal("expected error for nil ClientHelloInfo")
	}
}

func TestStoreImportAndGet(t *testing.T) {
	dir := t.TempDir()
	s := New(nil, dir)

	certPEM, keyPEM := generateSelfSignedCert(t, "example.com")

	meta, err := s.Import(context.Background(), "example.com", certPEM, keyPEM)
	if err != nil {
		t.Fatalf("import failed: %v", err)
	}

	if meta.Domain != "example.com" {
		t.Fatalf("expected domain=example.com, got %s", meta.Domain)
	}
	if meta.Source != SourceManual {
		t.Fatalf("expected source=manual, got %s", meta.Source)
	}
	if meta.Issuer != "example.com" {
		t.Fatalf("expected issuer=example.com, got %s", meta.Issuer)
	}
	if meta.NotBefore == "" {
		t.Fatal("expected non-empty not_before")
	}
	if meta.NotAfter == "" {
		t.Fatal("expected non-empty not_after")
	}
	if meta.KeyType == "" {
		t.Fatal("expected non-empty key_type")
	}

	// GetCertificate should return the imported cert
	hello := &tls.ClientHelloInfo{ServerName: "example.com"}
	cert, err := s.GetCertificate(hello)
	if err != nil {
		t.Fatalf("GetCertificate failed: %v", err)
	}
	if cert == nil {
		t.Fatal("expected non-nil certificate")
	}

	// Get metadata
	gotMeta, ok := s.Get("example.com")
	if !ok {
		t.Fatal("expected to find metadata for example.com")
	}
	if gotMeta.Domain != "example.com" {
		t.Fatalf("expected domain=example.com, got %s", gotMeta.Domain)
	}

	// Case-insensitive domain lookup
	_, ok = s.Get("EXAMPLE.COM")
	if !ok {
		t.Fatal("expected case-insensitive lookup")
	}
}

func TestStoreImportInvalidPEM(t *testing.T) {
	s := New(nil, "")

	_, err := s.Import(context.Background(), "bad.com", []byte("not-a-cert"), []byte("not-a-key"))
	if err == nil {
		t.Fatal("expected error for invalid PEM")
	}

	// Store should remain empty
	items := s.List()
	if len(items) != 0 {
		t.Fatalf("expected 0 items, got %d", len(items))
	}
}

func TestStoreImportMissingDomain(t *testing.T) {
	s := New(nil, "")
	_, err := s.Import(context.Background(), "", []byte("cert"), []byte("key"))
	if err == nil {
		t.Fatal("expected error for empty domain")
	}
}

func TestStoreImportMissingPEM(t *testing.T) {
	s := New(nil, "")
	_, err := s.Import(context.Background(), "example.com", nil, nil)
	if err == nil {
		t.Fatal("expected error for missing PEM data")
	}
}

func TestStoreList(t *testing.T) {
	s := New(nil, "")

	cert1, key1 := generateSelfSignedCert(t, "a.com")
	cert2, key2 := generateSelfSignedCert(t, "b.com")

	if _, err := s.Import(context.Background(), "a.com", cert1, key1); err != nil {
		t.Fatalf("import a.com: %v", err)
	}
	if _, err := s.Import(context.Background(), "b.com", cert2, key2); err != nil {
		t.Fatalf("import b.com: %v", err)
	}

	items := s.List()
	if len(items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(items))
	}
}

func TestStoreListEmpty(t *testing.T) {
	s := New(nil, "")
	items := s.List()
	if len(items) != 0 {
		t.Fatalf("expected 0 items, got %d", len(items))
	}
}

func TestStoreDelete(t *testing.T) {
	dir := t.TempDir()
	s := New(nil, dir)

	certPEM, keyPEM := generateSelfSignedCert(t, "delete-me.com")
	if _, err := s.Import(context.Background(), "delete-me.com", certPEM, keyPEM); err != nil {
		t.Fatalf("import: %v", err)
	}

	if err := s.Delete(context.Background(), "delete-me.com"); err != nil {
		t.Fatalf("delete: %v", err)
	}

	_, ok := s.Get("delete-me.com")
	if ok {
		t.Fatal("expected cert to be gone after delete")
	}

	_, err := s.GetCertificate(&tls.ClientHelloInfo{ServerName: "delete-me.com"})
	if err == nil {
		t.Fatal("expected GetCertificate to fail after delete")
	}
}

func TestStoreDeleteNotFound(t *testing.T) {
	s := New(nil, "")
	err := s.Delete(context.Background(), "nonexistent.com")
	if err == nil {
		t.Fatal("expected error for deleting nonexistent cert")
	}
}

func TestStoreObtainNoACME(t *testing.T) {
	s := New(nil, "")
	err := s.Obtain(context.Background(), []string{"example.com"})
	if err == nil {
		t.Fatal("expected error when no ACME manager")
	}
}

func TestStoreObtainNoDomains(t *testing.T) {
	s := New(nil, "")
	err := s.Obtain(context.Background(), nil)
	if err == nil {
		t.Fatal("expected error for no domains")
	}
}

func TestStoreLoadFromDisk(t *testing.T) {
	dir := t.TempDir()

	// Create a store, import a cert, persist
	s1 := New(nil, dir)
	certPEM, keyPEM := generateSelfSignedCert(t, "persist.com")
	if _, err := s1.Import(context.Background(), "persist.com", certPEM, keyPEM); err != nil {
		t.Fatalf("import: %v", err)
	}

	// Verify files were written
	if _, err := os.Stat(filepath.Join(dir, "certs.json")); err != nil {
		t.Fatalf("certs.json not found: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "manual", "persist.com.crt")); err != nil {
		t.Fatalf("manual cert not found: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "manual", "persist.com.key")); err != nil {
		t.Fatalf("manual key not found: %v", err)
	}

	// Create a new store and load from the same dir
	s2 := New(nil, dir)
	if err := s2.Load(context.Background()); err != nil {
		t.Fatalf("load: %v", err)
	}

	// Verify metadata loaded
	meta, ok := s2.Get("persist.com")
	if !ok {
		t.Fatal("expected to find persist.com after load")
	}
	if meta.Source != SourceManual {
		t.Fatalf("expected source=manual, got %s", meta.Source)
	}

	// Verify cert loaded
	cert, err := s2.GetCertificate(&tls.ClientHelloInfo{ServerName: "persist.com"})
	if err != nil {
		t.Fatalf("GetCertificate after load: %v", err)
	}
	if cert == nil {
		t.Fatal("expected non-nil cert after load")
	}
}

func TestStoreLoadEmptyDir(t *testing.T) {
	dir := t.TempDir()
	s := New(nil, dir)
	if err := s.Load(context.Background()); err != nil {
		t.Fatalf("load from empty dir: %v", err)
	}
}

func TestStoreLoadNoCacheDir(t *testing.T) {
	s := New(nil, "")
	if err := s.Load(context.Background()); err != nil {
		t.Fatalf("load with no cache dir: %v", err)
	}
}

func TestStoreGetNotFound(t *testing.T) {
	s := New(nil, "")
	_, ok := s.Get("nonexistent.com")
	if ok {
		t.Fatal("expected not found")
	}
}
