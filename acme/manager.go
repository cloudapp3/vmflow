package acme

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/acme"
)

type Manager struct {
	client        *acme.Client
	account       *ecdsa.PrivateKey
	solver        *HTTP01Solver // nil when using dns-01
	dnsSolver     *DNS01Solver  // nil when using http-01
	challengeType string        // "http-01" or "dns-01"
	certCache     map[string]*tls.Certificate
	cacheDir      string
	mu            sync.RWMutex
	onObtain      func(domains []string) // called after successful Obtain or renewal
}

func NewManager(cacheDir string, solver *HTTP01Solver) *Manager {
	return &Manager{
		client: &acme.Client{
			DirectoryURL: acme.LetsEncryptURL,
		},
		solver:        solver,
		challengeType: "http-01",
		certCache:     make(map[string]*tls.Certificate),
		cacheDir:      cacheDir,
	}
}

// NewManagerWithDNS creates a Manager that uses dns-01 challenges.
func NewManagerWithDNS(cacheDir string, dnsSolver *DNS01Solver) *Manager {
	return &Manager{
		client: &acme.Client{
			DirectoryURL: acme.LetsEncryptURL,
		},
		dnsSolver:     dnsSolver,
		challengeType: "dns-01",
		certCache:     make(map[string]*tls.Certificate),
		cacheDir:      cacheDir,
	}
}

// GetCertificate returns a TLS certificate for the given SNI name.
func (m *Manager) GetCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	if hello == nil {
		return nil, fmt.Errorf("nil ClientHelloInfo")
	}
	name := strings.ToLower(hello.ServerName)

	m.mu.RLock()
	cert, ok := m.certCache[name]
	m.mu.RUnlock()

	if ok {
		return cert, nil
	}

	// Try to load from disk
	if m.cacheDir != "" {
		if loaded, err := m.loadFromDisk(name); err == nil {
			m.mu.Lock()
			m.certCache[name] = loaded
			m.mu.Unlock()
			return loaded, nil
		}
	}

	return nil, fmt.Errorf("no certificate for %s", name)
}

// Obtain requests a certificate for the given domains via ACME.
func (m *Manager) Obtain(ctx context.Context, domains []string) error {
	if len(domains) == 0 {
		return fmt.Errorf("no domains specified")
	}

	// Try load from disk first
	if m.cacheDir != "" {
		if loaded, err := m.loadFromDisk(domains[0]); err == nil {
			m.mu.Lock()
			for _, d := range domains {
				m.certCache[d] = loaded
			}
			m.mu.Unlock()
			log.Printf("[acme] loaded cached certificate for %s", strings.Join(domains, ","))
			return nil
		}
	}

	// Register account if needed
	if m.account == nil {
		if err := m.registerAccount(ctx); err != nil {
			return fmt.Errorf("register acme account: %w", err)
		}
	}

	// Authorize each domain
	for _, domain := range domains {
		if err := m.authorize(ctx, domain); err != nil {
			return fmt.Errorf("authorize %s: %w", domain, err)
		}
	}

	// Create CSR and request certificate
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fmt.Errorf("generate key: %w", err)
	}

	csr, err := createCSR(key, domains)
	if err != nil {
		return fmt.Errorf("create CSR: %w", err)
	}

	der, _, err := m.client.CreateCert(ctx, csr, 90*24*time.Hour, true)
	if err != nil {
		return fmt.Errorf("create cert: %w", err)
	}

	cert := &tls.Certificate{
		Certificate: der,
		PrivateKey:  key,
		Leaf:        parseLeaf(der),
	}

	m.mu.Lock()
	for _, d := range domains {
		m.certCache[d] = cert
	}
	m.mu.Unlock()

	// Save to disk
	if m.cacheDir != "" {
		if err := m.saveToDisk(domains[0], cert); err != nil {
			log.Printf("[acme] warning: failed to cache cert: %v", err)
		}
	}

	log.Printf("[acme] obtained certificate for %s", strings.Join(domains, ","))
	if m.onObtain != nil {
		m.onObtain(domains)
	}
	return nil
}

// RenewLoop checks daily and renews certificates expiring within 30 days.
func (m *Manager) RenewLoop(ctx context.Context) {
	ticker := time.NewTicker(24 * time.Hour)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.renewExpiring(ctx)
		}
	}
}

func (m *Manager) renewExpiring(ctx context.Context) {
	m.mu.RLock()
	domains := make(map[string]*tls.Certificate, len(m.certCache))
	for k, v := range m.certCache {
		domains[k] = v
	}
	m.mu.RUnlock()

	seen := make(map[string]bool)
	for name, cert := range domains {
		if cert.Leaf == nil {
			continue
		}
		if seen[cert.Leaf.SerialNumber.String()] {
			continue
		}
		seen[cert.Leaf.SerialNumber.String()] = true

		if time.Until(cert.Leaf.NotAfter) > 30*24*time.Hour {
			continue
		}

		domainList := cert.Leaf.DNSNames
		if len(domainList) == 0 {
			domainList = []string{name}
		}

		log.Printf("[acme] renewing certificate for %s (expires %s)", strings.Join(domainList, ","), cert.Leaf.NotAfter.Format("2006-01-02"))
		if err := m.Obtain(ctx, domainList); err != nil {
			log.Printf("[acme] renewal failed: %v", err)
		}
	}
}

func (m *Manager) registerAccount(ctx context.Context) error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	m.client.Key = key
	acct := &acme.Account{Contact: []string{}}
	_, err = m.client.Register(ctx, acct, acme.AcceptTOS)
	if err != nil {
		if !strings.Contains(err.Error(), "already") {
			return err
		}
	}
	m.account = key
	return nil
}

func (m *Manager) authorize(ctx context.Context, domain string) error {
	authz, err := m.client.Authorize(ctx, domain)
	if err != nil {
		return err
	}

	// Find the matching challenge type
	var chal *acme.Challenge
	for _, c := range authz.Challenges {
		if c.Type == m.challengeType {
			chal = c
			break
		}
	}
	if chal == nil {
		return fmt.Errorf("no %s challenge for %s", m.challengeType, domain)
	}

	switch m.challengeType {
	case "http-01":
		response, err := m.client.HTTP01ChallengeResponse(chal.Token)
		if err != nil {
			return err
		}
		if err := m.solver.Present(ctx, chal.Token, response); err != nil {
			return err
		}
		defer m.solver.CleanUp(ctx, chal.Token)

	case "dns-01":
		value, err := m.client.DNS01ChallengeRecord(chal.Token)
		if err != nil {
			return err
		}
		fqdn := ChallengeFQDN(domain)
		if err := m.dnsSolver.Present(ctx, fqdn, value); err != nil {
			return err
		}
		defer m.dnsSolver.CleanUp(ctx, fqdn)
		// Wait for DNS propagation before accepting
		if err := m.dnsSolver.PropagationWait(ctx, fqdn, value); err != nil {
			return err
		}
	}

	// Accept challenge
	if _, err := m.client.Accept(ctx, chal); err != nil {
		return err
	}

	// Wait for authorization
	_, err = m.client.WaitAuthorization(ctx, authz.URI)
	return err
}

func (m *Manager) loadFromDisk(domain string) (*tls.Certificate, error) {
	certPath := filepath.Join(m.cacheDir, domain+".crt")
	keyPath := filepath.Join(m.cacheDir, domain+".key")

	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, err
	}
	if cert.Leaf == nil && len(cert.Certificate) > 0 {
		cert.Leaf, _ = x509.ParseCertificate(cert.Certificate[0])
	}
	return &cert, nil
}

func (m *Manager) saveToDisk(domain string, cert *tls.Certificate) error {
	if err := os.MkdirAll(m.cacheDir, 0700); err != nil {
		return err
	}

	// Save certificate
	certPath := filepath.Join(m.cacheDir, domain+".crt")
	var certPEM []byte
	for _, der := range cert.Certificate {
		certPEM = append(certPEM, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})...)
	}
	if err := os.WriteFile(certPath, certPEM, 0644); err != nil {
		return err
	}

	// Save private key
	keyPath := filepath.Join(m.cacheDir, domain+".key")
	keyBytes, err := x509.MarshalECPrivateKey(cert.PrivateKey.(*ecdsa.PrivateKey))
	if err != nil {
		return err
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyBytes})
	return os.WriteFile(keyPath, keyPEM, 0600)
}

func createCSR(key crypto.Signer, domains []string) ([]byte, error) {
	template := &x509.CertificateRequest{
		DNSNames: domains,
	}
	return x509.CreateCertificateRequest(rand.Reader, template, key)
}

func pkixSubject(name string) pkixName {
	return pkixName{CommonName: name}
}

type pkixName = struct{ CommonName string }

func parseLeaf(derChain [][]byte) *x509.Certificate {
	if len(derChain) == 0 {
		return nil
	}
	leaf, _ := x509.ParseCertificate(derChain[0])
	return leaf
}

// SetOnObtain registers a callback invoked after a certificate is successfully
// obtained or renewed. The callback receives the list of domains.
func (m *Manager) SetOnObtain(fn func(domains []string)) {
	m.onObtain = fn
}

// LoadedDomains returns the domains currently cached in memory.
func (m *Manager) LoadedDomains() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	domains := make([]string, 0, len(m.certCache))
	for d := range m.certCache {
		domains = append(domains, d)
	}
	return domains
}

// Preload reads cached certificates from disk into memory.
// This allows GetCertificate to serve them without lazy-loading.
func (m *Manager) Preload(ctx context.Context) {
	if m.cacheDir == "" {
		return
	}
	entries, err := os.ReadDir(m.cacheDir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".crt") {
			continue
		}
		domain := strings.TrimSuffix(entry.Name(), ".crt")
		m.GetCertificate(&tls.ClientHelloInfo{ServerName: domain})
	}
}
