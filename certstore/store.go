package certstore

import (
	"context"
	"crypto/ecdsa"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"log"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/cloudapp3/vmflow/acme"
	"github.com/cloudapp3/vmflow/internal/certpath"
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
	diskMu   sync.Mutex
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
	s.diskMu.Lock()
	defer s.diskMu.Unlock()

	data, err := certpath.ReadMetadata(s.cacheDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("read certs.json: %w", err)
	}

	var entries []*CertMeta
	if err := json.Unmarshal(data, &entries); err != nil {
		return fmt.Errorf("parse certs.json: %w", err)
	}

	loadedMeta := make(map[string]*CertMeta, len(entries))
	loadedCerts := make(map[string]*tls.Certificate, len(entries))
	for index, entry := range entries {
		if entry == nil {
			return fmt.Errorf("invalid certs.json entry %d: null entry", index+1)
		}
		domain, err := certpath.Normalize(entry.Domain, true)
		if err != nil {
			return fmt.Errorf("invalid certs.json entry %d: %w", index+1, err)
		}
		entry.Domain = domain
		if len(entry.SANs) > 0 {
			entry.SANs, err = certpath.NormalizeAll(entry.SANs, true)
			if err != nil {
				return fmt.Errorf("invalid certs.json SANs for %s: %w", domain, err)
			}
		}

		switch entry.Source {
		case SourceManual:
			cert, err := s.loadManualFromDisk(domain)
			if err != nil {
				log.Printf("[certstore] warning: failed to load manual cert for %s: %v", domain, err)
			} else {
				loadedCerts[domain] = cert
			}
		case SourceACME:
			for _, san := range entry.SANs {
				loadedMeta[san] = entry
			}
		default:
			return fmt.Errorf("invalid certificate source %q for %s", entry.Source, domain)
		}
		loadedMeta[domain] = entry
	}

	s.mu.Lock()
	s.meta = loadedMeta
	s.certs = loadedCerts
	s.mu.Unlock()

	log.Printf("[certstore] loaded %d certificate entries from disk", len(loadedMeta))
	return nil
}

// GetCertificate returns a TLS certificate for the given SNI name.
// Implements engine.CertProvider. Checks manual certs first, then ACME.
func (s *Store) GetCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	if hello == nil {
		return nil, fmt.Errorf("nil ClientHelloInfo")
	}
	name, err := certpath.Normalize(hello.ServerName, false)
	if err != nil {
		return nil, err
	}

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
	normalizedDomains, err := certpath.NormalizeAll(domains, true)
	if err != nil {
		return err
	}
	domains = normalizedDomains

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
	return s.recordACMECert(domains)
}

// Import stores a manually provided PEM certificate+key pair.
func (s *Store) Import(ctx context.Context, domain string, certPEM, keyPEM []byte) (*CertMeta, error) {
	canonicalDomain, err := certpath.Normalize(domain, true)
	if err != nil {
		return nil, err
	}
	domain = canonicalDomain
	if len(certPEM) == 0 || len(keyPEM) == 0 {
		return nil, fmt.Errorf("missing certificate or key PEM data")
	}

	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("invalid certificate/key pair: %w", err)
	}

	if len(cert.Certificate) == 0 {
		return nil, fmt.Errorf("certificate chain is empty")
	}
	if cert.Leaf == nil {
		cert.Leaf, err = x509.ParseCertificate(cert.Certificate[0])
		if err != nil {
			return nil, fmt.Errorf("parse certificate leaf: %w", err)
		}
	}
	if err := verifyCertificateDomain(cert.Leaf, domain); err != nil {
		return nil, err
	}

	meta := s.buildMeta(domain, &cert, SourceManual)
	s.diskMu.Lock()
	defer s.diskMu.Unlock()
	var previousCertPEM, previousKeyPEM []byte
	hadPreviousPair := false
	if s.cacheDir != "" {
		previousCertPEM, previousKeyPEM, err = certpath.ReadPair(s.cacheDir, certpath.Manual, domain)
		switch {
		case err == nil:
			hadPreviousPair = true
		case errors.Is(err, os.ErrNotExist):
		case err != nil:
			return nil, fmt.Errorf("inspect existing certificate pair: %w", err)
		}
		if err := certpath.WritePair(s.cacheDir, certpath.Manual, domain, certPEM, keyPEM); err != nil {
			return nil, err
		}
	}

	s.mu.Lock()
	oldMeta, hadMeta := s.meta[domain]
	oldCert, hadCert := s.certs[domain]
	s.meta[domain] = meta
	s.certs[domain] = &cert
	entries := s.metadataSnapshotLocked()
	s.mu.Unlock()

	if s.cacheDir != "" {
		if err := s.persistEntries(entries); err != nil {
			s.mu.Lock()
			if hadMeta {
				s.meta[domain] = oldMeta
			} else {
				delete(s.meta, domain)
			}
			if hadCert {
				s.certs[domain] = oldCert
			} else {
				delete(s.certs, domain)
			}
			s.mu.Unlock()
			var rollbackErr error
			if hadPreviousPair {
				rollbackErr = certpath.WritePair(s.cacheDir, certpath.Manual, domain, previousCertPEM, previousKeyPEM)
			} else {
				rollbackErr = certpath.RemovePair(s.cacheDir, certpath.Manual, domain)
			}
			if rollbackErr != nil {
				return nil, fmt.Errorf("persist certificate metadata: %v; restore certificate pair: %w", err, rollbackErr)
			}
			return nil, err
		}
	}

	log.Printf("[certstore] imported manual certificate for %s", domain)
	return meta, nil
}

// Delete removes a certificate. For ACME certs, removes metadata only.
// For manual certs, also removes PEM files from disk.
func (s *Store) Delete(ctx context.Context, domain string) error {
	canonicalDomain, err := certpath.Normalize(domain, true)
	if err != nil {
		return err
	}
	domain = canonicalDomain
	s.diskMu.Lock()
	defer s.diskMu.Unlock()

	s.mu.Lock()
	meta, ok := s.meta[domain]
	if !ok {
		s.mu.Unlock()
		return fmt.Errorf("certificate not found: %s", domain)
	}

	oldCert, hadCert := s.certs[domain]
	delete(s.meta, domain)
	delete(s.certs, domain)
	entries := s.metadataSnapshotLocked()
	s.mu.Unlock()

	rollback := func() {
		s.mu.Lock()
		s.meta[domain] = meta
		if hadCert {
			s.certs[domain] = oldCert
		}
		s.mu.Unlock()
	}
	if s.cacheDir != "" {
		if err := s.persistEntries(entries); err != nil {
			rollback()
			return err
		}
		if meta.Source == SourceManual {
			if err := certpath.RemovePair(s.cacheDir, certpath.Manual, domain); err != nil {
				rollback()
				if persistErr := s.persistEntries(s.metadataSnapshot()); persistErr != nil {
					return fmt.Errorf("remove certificate: %v; restore metadata: %w", err, persistErr)
				}
				return err
			}
		}
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
	canonicalDomain, err := certpath.Normalize(domain, true)
	if err != nil {
		return CertMeta{}, false
	}
	domain = canonicalDomain
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
	if err := s.recordACMECert(domains); err != nil {
		log.Printf("[certstore] warning: failed to refresh ACME metadata: %v", err)
	}
}

// recordACMECert extracts metadata from ACME-obtained certs and updates the store.
func (s *Store) recordACMECert(domains []string) error {
	normalizedDomains, err := certpath.NormalizeAll(domains, true)
	if err != nil {
		return err
	}
	domains = normalizedDomains

	s.mu.RLock()
	acmeMgr := s.acmeMgr
	s.mu.RUnlock()

	if acmeMgr == nil {
		return fmt.Errorf("no ACME manager configured")
	}

	// Get the cert from ACME manager for the primary domain
	primary := domains[0]
	hello := &tls.ClientHelloInfo{ServerName: primary}
	cert, err := acmeMgr.GetCertificate(hello)
	if err != nil {
		return fmt.Errorf("get ACME cert for metadata: %w", err)
	}

	meta := s.buildMeta(primary, cert, SourceACME)
	meta.AutoRenew = true
	meta.SANs = domains

	s.diskMu.Lock()
	defer s.diskMu.Unlock()

	s.mu.Lock()
	oldEntries := make(map[string]*CertMeta, len(domains))
	missingEntries := make(map[string]struct{}, len(domains))
	for _, d := range domains {
		if previous, exists := s.meta[d]; exists {
			oldEntries[d] = previous
		} else {
			missingEntries[d] = struct{}{}
		}
		s.meta[d] = meta
	}
	entries := s.metadataSnapshotLocked()
	s.mu.Unlock()

	if s.cacheDir != "" {
		if err := s.persistEntries(entries); err != nil {
			s.mu.Lock()
			for domain, previous := range oldEntries {
				s.meta[domain] = previous
			}
			for domain := range missingEntries {
				delete(s.meta, domain)
			}
			s.mu.Unlock()
			return err
		}
	}

	log.Printf("[certstore] recorded ACME certificate metadata for %s", strings.Join(domains, ","))
	return nil
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

func (s *Store) persistEntries(entries []*CertMeta) error {
	data, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal certificate metadata: %w", err)
	}
	if err := certpath.WriteMetadata(s.cacheDir, data); err != nil {
		return fmt.Errorf("persist certificate metadata: %w", err)
	}
	return nil
}

func (s *Store) metadataSnapshot() []*CertMeta {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.metadataSnapshotLocked()
}

func (s *Store) metadataSnapshotLocked() []*CertMeta {
	entries := make([]*CertMeta, 0, len(s.meta))
	seen := make(map[*CertMeta]struct{}, len(s.meta))
	for _, meta := range s.meta {
		if _, exists := seen[meta]; exists {
			continue
		}
		seen[meta] = struct{}{}
		copyMeta := *meta
		copyMeta.SANs = append([]string(nil), meta.SANs...)
		entries = append(entries, &copyMeta)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Domain < entries[j].Domain })
	return entries
}

// loadManualFromDisk loads a manual PEM certificate from cacheDir/manual/.
func (s *Store) loadManualFromDisk(domain string) (*tls.Certificate, error) {
	certPEM, keyPEM, err := certpath.ReadPair(s.cacheDir, certpath.Manual, domain)
	if err != nil {
		return nil, err
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, err
	}
	if cert.Leaf == nil && len(cert.Certificate) > 0 {
		cert.Leaf, _ = x509.ParseCertificate(cert.Certificate[0])
	}
	return &cert, nil
}

func verifyCertificateDomain(cert *x509.Certificate, domain string) error {
	if cert == nil {
		return fmt.Errorf("certificate leaf is missing")
	}
	if strings.HasPrefix(domain, "*.") {
		for _, candidate := range cert.DNSNames {
			normalized, err := certpath.Normalize(candidate, true)
			if err == nil && normalized == domain {
				return nil
			}
		}
		return fmt.Errorf("certificate does not contain wildcard domain %s", domain)
	}
	if err := cert.VerifyHostname(domain); err != nil {
		return fmt.Errorf("certificate does not cover domain %s: %w", domain, err)
	}
	return nil
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
