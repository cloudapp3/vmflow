package certstore

import (
	"context"
	"crypto/ecdsa"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/cloudapp3/vmflow/acme"
)

// Source identifies how a certificate was obtained.
type Source string

const (
	SourceACME   Source = "acme"
	SourceManual Source = "manual"
)

// CertMeta is metadata for a stored certificate.
type CertMeta struct {
	Domain    string   `json:"domain"`
	SANs      []string `json:"sans,omitempty"`
	Issuer    string   `json:"issuer,omitempty"`
	NotBefore string   `json:"not_before,omitempty"`
	NotAfter  string   `json:"not_after,omitempty"`
	Source    Source   `json:"source"`
	KeyType   string   `json:"key_type,omitempty"`
	AutoRenew bool     `json:"auto_reew"`
}

// Store implements engine.CertProvider and provides certificate management
// capabilities including manual PEM import and metadata tracking.
type Store struct {
	mu       sync.RWMutex
	meta     map[string]*CertMeta
	certs    map[string]*tls.Certificate // manual certs live here
	acmeMgr  *acme.Manager               // nil when ACME is not configured
	cacheDir string
}

// New creates a certificate store.
// acmeMgr may be nil (manual-only mode).
// cacheDir is used for persistence; empty means no persistence.
func New(acmeMgr *acme.Manager, cacheDir string) *Store {
	return &Store{
		meta:     make(map[string]*CertMeta),
		certs:    make(map[string]*tls.Certificate),
		acmeMgr:  acmeMgr,
		cacheDir: cacheDir,
	}
}

// Load reads persisted metadata and manual certificates from disk.
// Call this once after New, before first use.
func (s *Store) Load(ctx context.Context) error {
	if s.cacheDir == "" {
		return nil
	}

	// Load metadata
	metaPath := filepath.Join(s.cacheDir, "certs.json")
	data, err := os.ReadFile(metaPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read certs.json: %w", err)
	}

	var entries []*CertMeta
	if err := json.Unmarshal(data, &entries); err != nil {
		return fmt.Errorf("parse certs.json: %w", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	for _, entry := range entries {
		domain := strings.ToLower(strings.TrimSpace(entry.Domain))
		if domain == "" {
			continue
		}

		s.meta[domain] = entry

		switch entry.Source {
		case SourceManual:
			// Load PEM files from cacheDir/manual/
			cert, err := s.loadManualFromDisk(domain)
			if err != nil {
				log.Printf("[certstore] warning: failed to load manual cert for %s: %v", domain, err)
				continue
			}
			s.certs[domain] = cert

		case SourceACME:
			// ACME certs are loaded by acme.Manager on demand via GetCertificate.
			// We just keep the metadata.
		}
	}

	log.Printf("[certstore] loaded %d certificate entries from disk", len(s.meta))
	return nil
}

// GetCertificate returns a TLS certificate for the given SNI name.
// Implements engine.CertProvider. Checks manual certs first, then ACME.
func (s *Store) GetCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	if hello == nil {
		return nil, fmt.Errorf("nil ClientHelloInfo")
	}
	name := strings.ToLower(hello.ServerName)

	s.mu.RLock()
	// Check manual certs first
	if cert, ok := s.certs[name]; ok {
		s.mu.RUnlock()
		return cert, nil
	}
	acmeMgr := s.acmeMgr
	s.mu.RUnlock()

	// Delegate to ACME manager
	if acmeMgr != nil {
		return acmeMgr.GetCertificate(hello)
	}

	return nil, fmt.Errorf("no certificate for %s", name)
}

// Obtain requests a certificate via ACME for the given domains.
// Implements engine.CertProvider.
func (s *Store) Obtain(ctx context.Context, domains []string) error {
	if len(domains) == 0 {
		return fmt.Errorf("no domains specified")
	}

	s.mu.RLock()
	acmeMgr := s.acmeMgr
	s.mu.RUnlock()

	if acmeMgr == nil {
		return fmt.Errorf("no ACME manager configured; cannot obtain certificate for %s", strings.Join(domains, ","))
	}

	// Delegate to ACME manager
	if err := acmeMgr.Obtain(ctx, domains); err != nil {
		return err
	}

	// Record metadata from the obtained certificate
	s.recordACMECert(domains)

	return nil
}

// Import stores a manually provided PEM certificate+key pair.
func (s *Store) Import(ctx context.Context, domain string, certPEM, keyPEM []byte) (*CertMeta, error) {
	domain = strings.ToLower(strings.TrimSpace(domain))
	if domain == "" {
		return nil, fmt.Errorf("missing domain")
	}
	if len(certPEM) == 0 || len(keyPEM) == 0 {
		return nil, fmt.Errorf("missing certificate or key PEM data")
	}

	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("invalid certificate/key pair: %w", err)
	}

	// Parse leaf for metadata
	if cert.Leaf == nil && len(cert.Certificate) > 0 {
		cert.Leaf, _ = x509.ParseCertificate(cert.Certificate[0])
	}

	meta := s.buildMeta(domain, &cert, SourceManual)

	s.mu.Lock()
	s.meta[domain] = meta
	s.certs[domain] = &cert
	s.mu.Unlock()

	// Persist to disk
	if s.cacheDir != "" {
		manualDir := filepath.Join(s.cacheDir, "manual")
		if err := os.MkdirAll(manualDir, 0700); err != nil {
			log.Printf("[certstore] warning: failed to create manual cert dir: %v", err)
		} else {
			certPath := filepath.Join(manualDir, domain+".crt")
			keyPath := filepath.Join(manualDir, domain+".key")
			if err := os.WriteFile(certPath, certPEM, 0644); err != nil {
				log.Printf("[certstore] warning: failed to write cert: %v", err)
			}
			if err := os.WriteFile(keyPath, keyPEM, 0600); err != nil {
				log.Printf("[certstore] warning: failed to write key: %v", err)
			}
		}
		s.persist()
	}

	log.Printf("[certstore] imported manual certificate for %s", domain)
	return meta, nil
}

// Delete removes a certificate. For ACME certs, removes metadata only.
// For manual certs, also removes PEM files from disk.
func (s *Store) Delete(ctx context.Context, domain string) error {
	domain = strings.ToLower(strings.TrimSpace(domain))
	if domain == "" {
		return fmt.Errorf("missing domain")
	}

	s.mu.Lock()
	meta, ok := s.meta[domain]
	if !ok {
		s.mu.Unlock()
		return fmt.Errorf("certificate not found: %s", domain)
	}

	delete(s.meta, domain)
	delete(s.certs, domain)
	s.mu.Unlock()

	// Remove manual PEM files
	if meta.Source == SourceManual && s.cacheDir != "" {
		manualDir := filepath.Join(s.cacheDir, "manual")
		os.Remove(filepath.Join(manualDir, domain+".crt"))
		os.Remove(filepath.Join(manualDir, domain+".key"))
		s.persist()
	}

	log.Printf("[certstore] deleted certificate for %s (source=%s)", domain, meta.Source)
	return nil
}

// List returns metadata for all stored certificates.
func (s *Store) List() []CertMeta {
	s.mu.RLock()
	defer s.mu.RUnlock()

	result := make([]CertMeta, 0, len(s.meta))
	for _, m := range s.meta {
		result = append(result, *m)
	}
	return result
}

// Get returns metadata for a single domain.
func (s *Store) Get(domain string) (CertMeta, bool) {
	domain = strings.ToLower(strings.TrimSpace(domain))
	s.mu.RLock()
	defer s.mu.RUnlock()

	m, ok := s.meta[domain]
	if !ok {
		return CertMeta{}, false
	}
	return *m, true
}

// RefreshACME updates metadata for ACME-obtained certs after renewal.
// Called by the acme.Manager onObtain callback.
func (s *Store) RefreshACME(domains []string) {
	if len(domains) == 0 {
		return
	}
	s.recordACMECert(domains)
}

// recordACMECert extracts metadata from ACME-obtained certs and updates the store.
func (s *Store) recordACMECert(domains []string) {
	s.mu.RLock()
	acmeMgr := s.acmeMgr
	s.mu.RUnlock()

	if acmeMgr == nil {
		return
	}

	// Get the cert from ACME manager for the primary domain
	primary := domains[0]
	hello := &tls.ClientHelloInfo{ServerName: primary}
	cert, err := acmeMgr.GetCertificate(hello)
	if err != nil {
		log.Printf("[certstore] warning: failed to get ACME cert for metadata: %v", err)
		return
	}

	meta := s.buildMeta(primary, cert, SourceACME)
	meta.AutoRenew = true
	meta.SANs = domains

	s.mu.Lock()
	for _, d := range domains {
		s.meta[strings.ToLower(d)] = meta
	}
	s.mu.Unlock()

	if s.cacheDir != "" {
		s.persist()
	}

	log.Printf("[certstore] recorded ACME certificate metadata for %s", strings.Join(domains, ","))
}

// buildMeta creates CertMeta from a tls.Certificate.
func (s *Store) buildMeta(domain string, cert *tls.Certificate, source Source) *CertMeta {
	meta := &CertMeta{
		Domain: domain,
		Source: source,
	}

	if cert.Leaf != nil {
		meta.Issuer = cert.Leaf.Issuer.CommonName
		meta.NotBefore = cert.Leaf.NotBefore.Format(time.RFC3339)
		meta.NotAfter = cert.Leaf.NotAfter.Format(time.RFC3339)
		if len(cert.Leaf.DNSNames) > 0 {
			meta.SANs = cert.Leaf.DNSNames
		}
		meta.KeyType = keyType(cert.PrivateKey)
	}

	return meta
}

// persist writes metadata to disk as certs.json.
func (s *Store) persist() {
	if s.cacheDir == "" {
		return
	}

	s.mu.RLock()
	entries := make([]*CertMeta, 0, len(s.meta))
	for _, m := range s.meta {
		entries = append(entries, m)
	}
	s.mu.RUnlock()

	data, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		log.Printf("[certstore] warning: failed to marshal metadata: %v", err)
		return
	}

	if err := os.MkdirAll(s.cacheDir, 0700); err != nil {
		log.Printf("[certstore] warning: failed to create cache dir: %v", err)
		return
	}

	metaPath := filepath.Join(s.cacheDir, "certs.json")
	if err := os.WriteFile(metaPath, data, 0644); err != nil {
		log.Printf("[certstore] warning: failed to write certs.json: %v", err)
	}
}

// loadManualFromDisk loads a manual PEM certificate from cacheDir/manual/.
func (s *Store) loadManualFromDisk(domain string) (*tls.Certificate, error) {
	manualDir := filepath.Join(s.cacheDir, "manual")
	certPath := filepath.Join(manualDir, domain+".crt")
	keyPath := filepath.Join(manualDir, domain+".key")

	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, err
	}
	if cert.Leaf == nil && len(cert.Certificate) > 0 {
		cert.Leaf, _ = x509.ParseCertificate(cert.Certificate[0])
	}
	return &cert, nil
}

// keyType returns a human-readable key algorithm string.
func keyType(key any) string {
	switch k := key.(type) {
	case *ecdsa.PrivateKey:
		return fmt.Sprintf("ECDSA %s", k.Curve.Params().Name)
	case *rsa.PrivateKey:
		return fmt.Sprintf("RSA %d", k.N.BitLen())
	default:
		return "unknown"
	}
}

// DecodePEM is a helper to decode PEM blocks from raw bytes.
func DecodePEM(data []byte) ([]*pem.Block, error) {
	var blocks []*pem.Block
	rest := data
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		blocks = append(blocks, block)
	}
	if len(blocks) == 0 {
		return nil, fmt.Errorf("no PEM blocks found")
	}
	return blocks, nil
}
