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
	"runtime"
	"testing"
	"time"

	"github.com/cloudapp3/vmflow/internal/certpath"
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
	entries, err := os.ReadDir(filepath.Join(dir, "manual"))
	if err != nil {
		t.Fatalf("read manual certificate directory: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("stored certificate file count = %d, want 2", len(entries))
	}
	for _, entry := range entries {
		if entry.Name() == "persist.com.crt" || entry.Name() == "persist.com.key" {
			t.Fatalf("certificate domain leaked into storage filename %q", entry.Name())
		}
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

func TestStoreImportRejectsUnsafeDomain(t *testing.T) {
	dir := t.TempDir()
	s := New(nil, dir)
	certPEM, keyPEM := generateSelfSignedCert(t, "example.com")
	outside := filepath.Join(filepath.Dir(dir), "outside-sentinel")
	if err := os.WriteFile(outside, []byte("unchanged"), 0o600); err != nil {
		t.Fatalf("write sentinel: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(outside) })

	for _, domain := range []string{"../../outside-sentinel", `..\..\outside-sentinel`, "/tmp/outside-sentinel", `C:\outside-sentinel`} {
		if _, err := s.Import(context.Background(), domain, certPEM, keyPEM); err == nil {
			t.Errorf("Import(%q) unexpectedly succeeded", domain)
		}
	}
	data, err := os.ReadFile(outside)
	if err != nil {
		t.Fatalf("read sentinel: %v", err)
	}
	if string(data) != "unchanged" {
		t.Fatalf("outside sentinel changed to %q", data)
	}
	if len(s.List()) != 0 {
		t.Fatalf("unsafe imports changed in-memory metadata: %+v", s.List())
	}
}

func TestStoreImportRejectsCertificateForDifferentDomain(t *testing.T) {
	s := New(nil, t.TempDir())
	certPEM, keyPEM := generateSelfSignedCert(t, "example.com")
	if _, err := s.Import(context.Background(), "other.example.com", certPEM, keyPEM); err == nil {
		t.Fatal("Import unexpectedly accepted a certificate for a different domain")
	}
	if len(s.List()) != 0 {
		t.Fatalf("mismatched import changed metadata: %+v", s.List())
	}
}

func TestStoreDeleteRejectsUnsafeDomain(t *testing.T) {
	dir := t.TempDir()
	s := New(nil, dir)
	outside := filepath.Join(filepath.Dir(dir), "outside-delete-sentinel")
	if err := os.WriteFile(outside, []byte("unchanged"), 0o600); err != nil {
		t.Fatalf("write sentinel: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(outside) })

	for _, domain := range []string{"../../outside-delete-sentinel", `..\..\outside-delete-sentinel`, "/tmp/outside-delete-sentinel"} {
		if err := s.Delete(context.Background(), domain); err == nil {
			t.Errorf("Delete(%q) unexpectedly succeeded", domain)
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

func TestStoreLoadRejectsUnsafeMetadataDomain(t *testing.T) {
	dir := t.TempDir()
	metadata := `[{"domain":"../../outside","source":"manual"}]`
	if err := os.WriteFile(filepath.Join(dir, "certs.json"), []byte(metadata), 0o600); err != nil {
		t.Fatalf("write metadata: %v", err)
	}
	s := New(nil, dir)
	if err := s.Load(context.Background()); err == nil {
		t.Fatal("Load unexpectedly accepted an unsafe metadata domain")
	}
	if len(s.List()) != 0 {
		t.Fatalf("unsafe metadata changed store state: %+v", s.List())
	}
}

func TestStoreLoadMigratesLegacyDomainFilenames(t *testing.T) {
	dir := t.TempDir()
	manualDir := filepath.Join(dir, "manual")
	if err := os.MkdirAll(manualDir, 0o700); err != nil {
		t.Fatalf("create manual directory: %v", err)
	}
	certPEM, keyPEM := generateSelfSignedCert(t, "legacy.example.com")
	legacyCert := filepath.Join(manualDir, "legacy.example.com.crt")
	legacyKey := filepath.Join(manualDir, "legacy.example.com.key")
	if err := os.WriteFile(legacyCert, certPEM, 0o644); err != nil {
		t.Fatalf("write legacy cert: %v", err)
	}
	if err := os.WriteFile(legacyKey, keyPEM, 0o600); err != nil {
		t.Fatalf("write legacy key: %v", err)
	}
	metadata := `[{"domain":"legacy.example.com","source":"manual"}]`
	if err := os.WriteFile(filepath.Join(dir, "certs.json"), []byte(metadata), 0o600); err != nil {
		t.Fatalf("write metadata: %v", err)
	}

	s := New(nil, dir)
	if err := s.Load(context.Background()); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, err := s.GetCertificate(&tls.ClientHelloInfo{ServerName: "legacy.example.com"}); err != nil {
		t.Fatalf("GetCertificate after migration: %v", err)
	}
	if _, err := os.Lstat(legacyCert); !os.IsNotExist(err) {
		t.Fatalf("legacy cert still exists: %v", err)
	}
	if _, err := os.Lstat(legacyKey); !os.IsNotExist(err) {
		t.Fatalf("legacy key still exists: %v", err)
	}
}

func TestStoreLoadRestoresACMESANAliases(t *testing.T) {
	dir := t.TempDir()
	metadata := `[{
  "domain":"example.com",
  "sans":["example.com","api.example.com"],
  "source":"acme"
}]`
	if err := os.WriteFile(filepath.Join(dir, "certs.json"), []byte(metadata), 0o600); err != nil {
		t.Fatalf("write metadata: %v", err)
	}
	s := New(nil, dir)
	if err := s.Load(context.Background()); err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, domain := range []string{"example.com", "api.example.com"} {
		meta, ok := s.Get(domain)
		if !ok || meta.Domain != "example.com" {
			t.Fatalf("Get(%q) = %+v, %v", domain, meta, ok)
		}
	}
}

func TestStoreImportRestoresPairWhenMetadataPersistenceFails(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires platform-specific privileges on Windows")
	}
	dir := t.TempDir()
	s := New(nil, dir)
	oldCertPEM, oldKeyPEM := generateSelfSignedCert(t, "example.com")
	if _, err := s.Import(context.Background(), "example.com", oldCertPEM, oldKeyPEM); err != nil {
		t.Fatalf("initial Import: %v", err)
	}

	metadataPath := filepath.Join(dir, "certs.json")
	if err := os.Remove(metadataPath); err != nil {
		t.Fatalf("remove metadata: %v", err)
	}
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("unchanged"), 0o600); err != nil {
		t.Fatalf("write outside file: %v", err)
	}
	if err := os.Symlink(outside, metadataPath); err != nil {
		t.Fatalf("symlink metadata: %v", err)
	}

	newCertPEM, newKeyPEM := generateSelfSignedCert(t, "example.com")
	if _, err := s.Import(context.Background(), "example.com", newCertPEM, newKeyPEM); err == nil {
		t.Fatal("replacement Import unexpectedly succeeded with unsafe metadata path")
	}
	storedCertPEM, storedKeyPEM, err := certpath.ReadPair(dir, certpath.Manual, "example.com")
	if err != nil {
		t.Fatalf("ReadPair after rollback: %v", err)
	}
	if string(storedCertPEM) != string(oldCertPEM) || string(storedKeyPEM) != string(oldKeyPEM) {
		t.Fatal("failed replacement did not restore the previous certificate pair")
	}
	outsideData, err := os.ReadFile(outside)
	if err != nil {
		t.Fatalf("read outside file: %v", err)
	}
	if string(outsideData) != "unchanged" {
		t.Fatalf("outside file changed to %q", outsideData)
	}
}
