package certreview

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"testing"
	"time"

	"github.com/cloudapp3/vmflow/certstore"
	"github.com/cloudapp3/vmflow/engine"
)

func generateCertPEM(t *testing.T, domain string, notBefore, notAfter time.Time) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: domain},
		DNSNames:     []string{domain},
		NotBefore:    notBefore,
		NotAfter:     notAfter,
	}
	certDER, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	keyDER, _ := x509.MarshalECPrivateKey(key)
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return
}

func generateRSACertPEM(t *testing.T, domain string, bits int, notBefore, notAfter time.Time) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, bits)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: domain},
		DNSNames:     []string{domain},
		NotBefore:    notBefore,
		NotAfter:     notAfter,
	}
	certDER, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	keyDER := x509.MarshalPKCS1PrivateKey(key)
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: keyDER})
	return
}

func generateMultiSANCertPEM(t *testing.T, domains []string, notBefore, notAfter time.Time) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: domains[0]},
		DNSNames:     domains,
		NotBefore:    notBefore,
		NotAfter:     notAfter,
	}
	certDER, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	keyDER, _ := x509.MarshalECPrivateKey(key)
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return
}

func TestReviewClean(t *testing.T) {
	dir := t.TempDir()
	store := certstore.New(nil, dir)

	certPEM, keyPEM := generateCertPEM(t, "healthy.com",
		time.Now().Add(-1*time.Hour), time.Now().Add(90*24*time.Hour))
	if _, err := store.Import(t.Context(), "healthy.com", certPEM, keyPEM); err != nil {
		t.Fatalf("import: %v", err)
	}

	rules := []engine.Rule{{
		RuleID:   "r1",
		Name:     "r1",
		Protocol: engine.ProtocolHTTPS,
		Enabled:  true,
		Domains:  []string{"healthy.com"},
	}}

	reviewer := NewReviewer(store, func() []engine.Rule { return rules }, DefaultOptions())
	result := reviewer.Review()

	if !result.OK {
		t.Fatalf("expected OK, got findings: %+v", result.Findings)
	}
	if result.Critical != 0 {
		t.Fatalf("expected 0 critical, got %d", result.Critical)
	}
	if result.Total != 0 {
		t.Fatalf("expected 0 findings, got %d", result.Total)
	}
}

func TestReviewExpiryWarning(t *testing.T) {
	dir := t.TempDir()
	store := certstore.New(nil, dir)

	certPEM, keyPEM := generateCertPEM(t, "expiring-soon.com",
		time.Now().Add(-60*24*time.Hour), time.Now().Add(20*24*time.Hour))
	if _, err := store.Import(t.Context(), "expiring-soon.com", certPEM, keyPEM); err != nil {
		t.Fatalf("import: %v", err)
	}

	reviewer := NewReviewer(store, func() []engine.Rule { return nil }, DefaultOptions())
	result := reviewer.Review()

	if result.Warnings == 0 {
		t.Fatal("expected at least 1 warning")
	}
	found := false
	for _, f := range result.Findings {
		if f.Check == "expiry_warning" && f.Domain == "expiring-soon.com" {
			found = true
			if f.Severity != SeverityWarning {
				t.Fatalf("expected warning severity, got %s", f.Severity)
			}
		}
	}
	if !found {
		t.Fatalf("expected expiry_warning finding, got: %+v", result.Findings)
	}
}

func TestReviewExpiryCritical(t *testing.T) {
	dir := t.TempDir()
	store := certstore.New(nil, dir)

	certPEM, keyPEM := generateCertPEM(t, "about-to-expire.com",
		time.Now().Add(-85*24*time.Hour), time.Now().Add(3*24*time.Hour))
	if _, err := store.Import(t.Context(), "about-to-expire.com", certPEM, keyPEM); err != nil {
		t.Fatalf("import: %v", err)
	}

	reviewer := NewReviewer(store, func() []engine.Rule { return nil }, DefaultOptions())
	result := reviewer.Review()

	if result.OK {
		t.Fatal("expected not OK due to critical")
	}
	if result.Critical == 0 {
		t.Fatal("expected at least 1 critical")
	}
	found := false
	for _, f := range result.Findings {
		if f.Check == "expiry_critical" && f.Domain == "about-to-expire.com" {
			found = true
			if f.Severity != SeverityCritical {
				t.Fatalf("expected critical severity, got %s", f.Severity)
			}
		}
	}
	if !found {
		t.Fatalf("expected expiry_critical finding, got: %+v", result.Findings)
	}
}

func TestReviewExpired(t *testing.T) {
	dir := t.TempDir()
	store := certstore.New(nil, dir)

	certPEM, keyPEM := generateCertPEM(t, "expired.com",
		time.Now().Add(-100*24*time.Hour), time.Now().Add(-1*time.Hour))
	if _, err := store.Import(t.Context(), "expired.com", certPEM, keyPEM); err != nil {
		t.Fatalf("import: %v", err)
	}

	reviewer := NewReviewer(store, func() []engine.Rule { return nil }, DefaultOptions())
	result := reviewer.Review()

	if result.Critical == 0 {
		t.Fatal("expected critical for expired cert")
	}
}

func TestReviewDomainMismatch(t *testing.T) {
	dir := t.TempDir()
	store := certstore.New(nil, dir)

	// No cert imported for missing.example.com
	rules := []engine.Rule{{
		RuleID:   "r1",
		Name:     "r1",
		Protocol: engine.ProtocolHTTPS,
		Enabled:  true,
		Domains:  []string{"missing.example.com"},
	}}

	reviewer := NewReviewer(store, func() []engine.Rule { return rules }, DefaultOptions())
	result := reviewer.Review()

	if result.Critical == 0 {
		t.Fatal("expected critical for domain mismatch")
	}
	found := false
	for _, f := range result.Findings {
		if f.Check == "domain_mismatch" && f.Domain == "missing.example.com" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected domain_mismatch finding, got: %+v", result.Findings)
	}
}

func TestReviewOrphanCert(t *testing.T) {
	dir := t.TempDir()
	store := certstore.New(nil, dir)

	certPEM, keyPEM := generateCertPEM(t, "orphan.com",
		time.Now().Add(-1*time.Hour), time.Now().Add(90*24*time.Hour))
	if _, err := store.Import(t.Context(), "orphan.com", certPEM, keyPEM); err != nil {
		t.Fatalf("import: %v", err)
	}

	// No rules reference orphan.com
	reviewer := NewReviewer(store, func() []engine.Rule { return nil }, DefaultOptions())
	result := reviewer.Review()

	if result.Info == 0 {
		t.Fatal("expected info for orphan cert")
	}
	found := false
	for _, f := range result.Findings {
		if f.Check == "orphan_cert" && f.Domain == "orphan.com" {
			found = true
			if f.Severity != SeverityInfo {
				t.Fatalf("expected info severity, got %s", f.Severity)
			}
		}
	}
	if !found {
		t.Fatalf("expected orphan_cert finding, got: %+v", result.Findings)
	}
}

func TestReviewKeyStrengthWeakRSA(t *testing.T) {
	dir := t.TempDir()
	store := certstore.New(nil, dir)

	certPEM, keyPEM := generateRSACertPEM(t, "weak.com", 1024,
		time.Now().Add(-1*time.Hour), time.Now().Add(90*24*time.Hour))
	if _, err := store.Import(t.Context(), "weak.com", certPEM, keyPEM); err != nil {
		t.Fatalf("import: %v", err)
	}

	reviewer := NewReviewer(store, func() []engine.Rule { return nil }, DefaultOptions())
	result := reviewer.Review()

	if result.Warnings == 0 {
		t.Fatal("expected warning for weak RSA key")
	}
	found := false
	for _, f := range result.Findings {
		if f.Check == "key_too_weak" && f.Domain == "weak.com" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected key_too_weak finding, got: %+v", result.Findings)
	}
}

func TestReviewSANMismatch(t *testing.T) {
	dir := t.TempDir()
	store := certstore.New(nil, dir)

	// Cert only covers san-test.com, not www.san-test.com
	certPEM, keyPEM := generateCertPEM(t, "san-test.com",
		time.Now().Add(-1*time.Hour), time.Now().Add(90*24*time.Hour))
	if _, err := store.Import(t.Context(), "san-test.com", certPEM, keyPEM); err != nil {
		t.Fatalf("import: %v", err)
	}

	rules := []engine.Rule{{
		RuleID:   "r1",
		Name:     "r1",
		Protocol: engine.ProtocolHTTPS,
		Enabled:  true,
		Domains:  []string{"san-test.com", "www.san-test.com"},
	}}

	reviewer := NewReviewer(store, func() []engine.Rule { return rules }, DefaultOptions())
	result := reviewer.Review()

	found := false
	for _, f := range result.Findings {
		if f.Check == "san_mismatch" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected san_mismatch finding, got: %+v", result.Findings)
	}
}

func TestReviewSANMatchOK(t *testing.T) {
	dir := t.TempDir()
	store := certstore.New(nil, dir)

	// Cert covers both domains
	certPEM, keyPEM := generateMultiSANCertPEM(t, []string{"multi.com", "www.multi.com"},
		time.Now().Add(-1*time.Hour), time.Now().Add(90*24*time.Hour))
	if _, err := store.Import(t.Context(), "multi.com", certPEM, keyPEM); err != nil {
		t.Fatalf("import: %v", err)
	}

	rules := []engine.Rule{{
		RuleID:   "r1",
		Name:     "r1",
		Protocol: engine.ProtocolHTTPS,
		Enabled:  true,
		Domains:  []string{"multi.com", "www.multi.com"},
	}}

	reviewer := NewReviewer(store, func() []engine.Rule { return rules }, DefaultOptions())
	result := reviewer.Review()

	for _, f := range result.Findings {
		if f.Check == "san_mismatch" {
			t.Fatalf("unexpected san_mismatch: %+v", f)
		}
	}
}

func TestReviewNilStore(t *testing.T) {
	reviewer := NewReviewer(nil, func() []engine.Rule { return nil }, DefaultOptions())
	result := reviewer.Review()

	if !result.OK {
		t.Fatal("expected OK for nil store")
	}
}

func TestReviewDisabledRuleSkipped(t *testing.T) {
	dir := t.TempDir()
	store := certstore.New(nil, dir)

	// No cert imported
	rules := []engine.Rule{{
		RuleID:   "r1",
		Name:     "r1",
		Protocol: engine.ProtocolHTTPS,
		Enabled:  false, // disabled
		Domains:  []string{"disabled.com"},
	}}

	reviewer := NewReviewer(store, func() []engine.Rule { return rules }, DefaultOptions())
	result := reviewer.Review()

	for _, f := range result.Findings {
		if f.Check == "domain_mismatch" {
			t.Fatal("disabled rules should be skipped")
		}
	}
}

func TestReviewCustomThresholds(t *testing.T) {
	dir := t.TempDir()
	store := certstore.New(nil, dir)

	// Cert expires in 45 days — should NOT trigger warning with 60-day threshold
	certPEM, keyPEM := generateCertPEM(t, "custom.com",
		time.Now().Add(-1*time.Hour), time.Now().Add(45*24*time.Hour))
	if _, err := store.Import(t.Context(), "custom.com", certPEM, keyPEM); err != nil {
		t.Fatalf("import: %v", err)
	}

	opts := Options{ExpiryWarningDays: 60, ExpiryCriticalDays: 14, MinRSABits: 2048}
	reviewer := NewReviewer(store, func() []engine.Rule { return nil }, opts)
	result := reviewer.Review()

	found := false
	for _, f := range result.Findings {
		if f.Check == "expiry_warning" && f.Domain == "custom.com" {
			found = true
		}
	}
	if !found {
		t.Fatal("expected expiry_warning with 60-day threshold for cert expiring in 45 days")
	}
}

func TestReviewSorting(t *testing.T) {
	dir := t.TempDir()
	store := certstore.New(nil, dir)

	// Import an expired cert (critical) and a healthy one (will be orphan = info)
	certPEM1, keyPEM1 := generateCertPEM(t, "expired.com",
		time.Now().Add(-100*24*time.Hour), time.Now().Add(-1*time.Hour))
	if _, err := store.Import(t.Context(), "expired.com", certPEM1, keyPEM1); err != nil {
		t.Fatalf("import: %v", err)
	}

	certPEM2, keyPEM2 := generateCertPEM(t, "orphan.com",
		time.Now().Add(-1*time.Hour), time.Now().Add(90*24*time.Hour))
	if _, err := store.Import(t.Context(), "orphan.com", certPEM2, keyPEM2); err != nil {
		t.Fatalf("import: %v", err)
	}

	reviewer := NewReviewer(store, func() []engine.Rule { return nil }, DefaultOptions())
	result := reviewer.Review()

	if len(result.Findings) < 2 {
		t.Fatalf("expected at least 2 findings, got %d", len(result.Findings))
	}

	// Critical should come before info
	if result.Findings[0].Severity == SeverityInfo && result.Findings[1].Severity == SeverityCritical {
		t.Fatal("critical findings should be sorted before info")
	}
}
