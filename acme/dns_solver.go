package acme

import (
	"context"
	"fmt"
	"log"
	"net"
	"strings"
	"time"
)

// DNSProvider sets and removes DNS TXT records for ACME dns-01 challenges.
type DNSProvider interface {
	// Present adds a TXT record with the given value under the given FQDN.
	Present(ctx context.Context, fqdn, value string) error

	// CleanUp removes the TXT record previously added by Present.
	CleanUp(ctx context.Context, fqdn string) error
}

// DNS01Solver wraps a DNSProvider to solve dns-01 challenges.
// It handles record provisioning and DNS propagation waiting.
type DNS01Solver struct {
	provider            DNSProvider
	propagationTimeout  time.Duration
	pollingInterval     time.Duration
	propagationResolver *net.Resolver
}

// DNS01Options configures DNS-01 challenge behavior.
type DNS01Options struct {
	// PropagationTimeout is the maximum time to wait for DNS propagation.
	// Defaults to 120 seconds.
	PropagationTimeout time.Duration

	// PollingInterval is how often to check for DNS propagation.
	// Defaults to 5 seconds.
	PollingInterval time.Duration
}

// NewDNS01Solver creates a DNS-01 challenge solver backed by the given provider.
func NewDNS01Solver(provider DNSProvider, opts DNS01Options) *DNS01Solver {
	if opts.PropagationTimeout <= 0 {
		opts.PropagationTimeout = 120 * time.Second
	}
	if opts.PollingInterval <= 0 {
		opts.PollingInterval = 5 * time.Second
	}

	// Use a public DNS resolver for propagation checks
	resolver := &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			d := net.Dialer{Timeout: 5 * time.Second}
			return d.DialContext(ctx, "udp", "8.8.8.8:53")
		},
	}

	return &DNS01Solver{
		provider:            provider,
		propagationTimeout:  opts.PropagationTimeout,
		pollingInterval:     opts.PollingInterval,
		propagationResolver: resolver,
	}
}

// Present adds the DNS TXT record for the challenge.
// fqdn should be "_acme-challenge.<domain>".
func (s *DNS01Solver) Present(ctx context.Context, fqdn, value string) error {
	log.Printf("[acme] dns-01: presenting TXT record for %s", fqdn)
	if err := s.provider.Present(ctx, fqdn, value); err != nil {
		return fmt.Errorf("dns-01 present %s: %w", fqdn, err)
	}
	return nil
}

// CleanUp removes the DNS TXT record after the challenge is complete.
func (s *DNS01Solver) CleanUp(ctx context.Context, fqdn string) error {
	log.Printf("[acme] dns-01: cleaning up TXT record for %s", fqdn)
	if err := s.provider.CleanUp(ctx, fqdn); err != nil {
		log.Printf("[acme] dns-01: warning: cleanup failed for %s: %v", fqdn, err)
		return err
	}
	return nil
}

// PropagationWait polls DNS until the TXT record is visible or timeout.
func (s *DNS01Solver) PropagationWait(ctx context.Context, fqdn, expectedValue string) error {
	log.Printf("[acme] dns-01: waiting for propagation of %s (timeout %s)", fqdn, s.propagationTimeout)

	deadline := time.Now().Add(s.propagationTimeout)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		// Look up TXT records using public DNS
		records, err := s.propagationResolver.LookupTXT(ctx, fqdn)
		if err == nil {
			for _, r := range records {
				if strings.TrimSpace(r) == strings.TrimSpace(expectedValue) {
					log.Printf("[acme] dns-01: propagation confirmed for %s", fqdn)
					return nil
				}
			}
		}

		time.Sleep(s.pollingInterval)
	}

	return fmt.Errorf("dns-01: propagation timeout for %s after %s", fqdn, s.propagationTimeout)
}

// ChallengeFQDN returns the full DNS name for a dns-01 challenge.
// E.g. for domain "example.com" returns "_acme-challenge.example.com".
func ChallengeFQDN(domain string) string {
	return "_acme-challenge." + strings.TrimSuffix(domain, ".")
}
