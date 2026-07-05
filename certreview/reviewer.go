package certreview

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/cloudapp3/vmflow/certstore"
	"github.com/cloudapp3/vmflow/engine"
)

// Severity describes the urgency of a finding.
type Severity string

const (
	SeverityCritical Severity = "critical"
	SeverityWarning  Severity = "warning"
	SeverityInfo     Severity = "info"
)

// Finding is one certificate review result.
type Finding struct {
	Domain   string   `json:"domain"`
	Severity Severity `json:"severity"`
	Check    string   `json:"check"`
	Message  string   `json:"message"`
	RuleID   string   `json:"rule_id,omitempty"`
}

// ReviewResult is the output of a full certificate review.
type ReviewResult struct {
	OK         bool      `json:"ok"`
	Total      int       `json:"total"`
	Critical   int       `json:"critical"`
	Warnings   int       `json:"warnings"`
	Info       int       `json:"info"`
	Findings   []Finding `json:"findings"`
	ReviewedAt int64     `json:"reviewed_at"`
}

// Options controls certificate review behavior.
type Options struct {
	// ExpiryWarningDays is the number of days before expiry to trigger a warning.
	ExpiryWarningDays int

	// ExpiryCriticalDays is the number of days before expiry to trigger a critical alert.
	ExpiryCriticalDays int

	// MinRSABits is the minimum acceptable RSA key size.
	MinRSABits int
}

// DefaultOptions returns production-safe review defaults.
func DefaultOptions() Options {
	return Options{
		ExpiryWarningDays:  30,
		ExpiryCriticalDays: 7,
		MinRSABits:         2048,
	}
}

// Reviewer performs certificate health checks.
type Reviewer struct {
	store *certstore.Store
	rules func() []engine.Rule
	opts  Options
}

// NewReviewer creates a certificate reviewer.
// The rules function provides the current running rules (lazy, called at review time).
func NewReviewer(store *certstore.Store, rules func() []engine.Rule, opts Options) *Reviewer {
	if opts.ExpiryWarningDays <= 0 {
		opts.ExpiryWarningDays = DefaultOptions().ExpiryWarningDays
	}
	if opts.ExpiryCriticalDays <= 0 {
		opts.ExpiryCriticalDays = DefaultOptions().ExpiryCriticalDays
	}
	if opts.MinRSABits <= 0 {
		opts.MinRSABits = DefaultOptions().MinRSABits
	}
	return &Reviewer{
		store: store,
		rules: rules,
		opts:  opts,
	}
}

// Review executes all certificate health checks and returns the result.
func (r *Reviewer) Review() ReviewResult {
	if r.store == nil {
		return ReviewResult{
			OK:         true,
			ReviewedAt: time.Now().Unix(),
			Findings:   []Finding{},
		}
	}

	rev := &reviewer{
		store: r.store,
		rules: r.rules(),
		opts:  r.opts,
		now:   time.Now(),
	}

	rev.checkExpiry()
	rev.checkDomainMismatch()
	rev.checkOrphanCerts()
	rev.checkKeyStrength()
	rev.checkSANMismatch()

	result := ReviewResult{
		OK:         rev.critical == 0,
		Total:      len(rev.findings),
		Critical:   rev.critical,
		Warnings:   rev.warnings,
		Info:       rev.infoCount,
		Findings:   rev.findings,
		ReviewedAt: time.Now().Unix(),
	}
	if result.Findings == nil {
		result.Findings = []Finding{}
	}
	sortFindings(result.Findings)
	return result
}

type reviewer struct {
	store *certstore.Store
	rules []engine.Rule
	opts  Options
	now   time.Time

	findings  []Finding
	critical  int
	warnings  int
	infoCount int
}

func (rev *reviewer) addCritical(domain, check, message string) {
	rev.critical++
	rev.findings = append(rev.findings, Finding{
		Domain: domain, Severity: SeverityCritical, Check: check, Message: message,
	})
}

func (rev *reviewer) addWarning(domain, check, message string) {
	rev.warnings++
	rev.findings = append(rev.findings, Finding{
		Domain: domain, Severity: SeverityWarning, Check: check, Message: message,
	})
}

func (rev *reviewer) addInfo(domain, check, message string) {
	rev.infoCount++
	rev.findings = append(rev.findings, Finding{
		Domain: domain, Severity: SeverityInfo, Check: check, Message: message,
	})
}

// checkExpiry checks certificate expiration dates.
func (rev *reviewer) checkExpiry() {
	for _, meta := range rev.store.List() {
		if meta.NotAfter == "" {
			continue
		}
		expiry, err := time.Parse(time.RFC3339, meta.NotAfter)
		if err != nil {
			continue
		}
		remaining := expiry.Sub(rev.now)
		days := int(remaining.Hours() / 24)

		switch {
		case remaining <= 0:
			rev.addCritical(meta.Domain, "expiry_critical",
				fmt.Sprintf("certificate for %s has EXPIRED on %s", meta.Domain, expiry.Format("2006-01-02")))
		case days <= rev.opts.ExpiryCriticalDays:
			rev.addCritical(meta.Domain, "expiry_critical",
				fmt.Sprintf("certificate for %s expires in %d days (%s)", meta.Domain, days, expiry.Format("2006-01-02")))
		case days <= rev.opts.ExpiryWarningDays:
			rev.addWarning(meta.Domain, "expiry_warning",
				fmt.Sprintf("certificate for %s expires in %d days (%s)", meta.Domain, days, expiry.Format("2006-01-02")))
		}
	}
}

// checkDomainMismatch checks that every HTTPS rule has a matching certificate.
func (rev *reviewer) checkDomainMismatch() {
	for _, rule := range rev.rules {
		if rule.Protocol != engine.ProtocolHTTPS || !rule.Enabled {
			continue
		}
		for _, domain := range rule.Domains {
			domain = strings.ToLower(strings.TrimSpace(domain))
			if domain == "" {
				continue
			}
			_, ok := rev.store.Get(domain)
			if !ok {
				rev.addCritical(domain, "domain_mismatch",
					fmt.Sprintf("HTTPS rule %q requires domain %q but no certificate found", rule.RuleID, domain))
			}
		}
	}
}

// checkOrphanCerts finds certificates that no HTTPS rule references.
func (rev *reviewer) checkOrphanCerts() {
	// Collect all domains referenced by HTTPS rules
	referenced := make(map[string]bool)
	for _, rule := range rev.rules {
		if rule.Protocol != engine.ProtocolHTTPS {
			continue
		}
		for _, domain := range rule.Domains {
			referenced[strings.ToLower(strings.TrimSpace(domain))] = true
		}
	}

	for _, meta := range rev.store.List() {
		if !referenced[meta.Domain] {
			rev.addInfo(meta.Domain, "orphan_cert",
				fmt.Sprintf("certificate for %s exists but no HTTPS rule references this domain", meta.Domain))
		}
	}
}

// checkKeyStrength checks for weak key algorithms.
func (rev *reviewer) checkKeyStrength() {
	for _, meta := range rev.store.List() {
		if meta.KeyType == "" {
			continue
		}
		// Check RSA key size
		if strings.HasPrefix(meta.KeyType, "RSA ") {
			var bits int
			fmt.Sscanf(meta.KeyType, "RSA %d", &bits)
			if bits > 0 && bits < rev.opts.MinRSABits {
				rev.addWarning(meta.Domain, "key_too_weak",
					fmt.Sprintf("certificate for %s uses RSA-%d, minimum recommended is RSA-%d", meta.Domain, bits, rev.opts.MinRSABits))
			}
		}
	}
}

// checkSANMismatch checks that certificate SANs cover all rule domains.
func (rev *reviewer) checkSANMismatch() {
	for _, rule := range rev.rules {
		if rule.Protocol != engine.ProtocolHTTPS || !rule.Enabled {
			continue
		}
		if len(rule.Domains) <= 1 {
			continue // Single domain rules don't need SAN coverage check
		}

		for _, domain := range rule.Domains {
			domain = strings.ToLower(strings.TrimSpace(domain))
			if domain == "" {
				continue
			}
			meta, ok := rev.store.Get(domain)
			if !ok {
				continue // Already caught by domain_mismatch
			}

			// Build SAN set
			sanSet := make(map[string]bool, len(meta.SANs))
			for _, san := range meta.SANs {
				sanSet[strings.ToLower(san)] = true
			}

			// Check all rule domains are covered
			var missing []string
			for _, d := range rule.Domains {
				d = strings.ToLower(strings.TrimSpace(d))
				if d != "" && !sanSet[d] {
					missing = append(missing, d)
				}
			}

			if len(missing) > 0 {
				rev.addWarning(domain, "san_mismatch",
					fmt.Sprintf("certificate for %s does not cover all rule domains: missing %s", domain, strings.Join(missing, ", ")))
			}
		}
	}
}

// sortFindings sorts findings by severity, then check, then domain.
func sortFindings(findings []Finding) {
	severityOrder := map[Severity]int{
		SeverityCritical: 0,
		SeverityWarning:  1,
		SeverityInfo:     2,
	}
	sort.SliceStable(findings, func(i, j int) bool {
		si, sj := severityOrder[findings[i].Severity], severityOrder[findings[j].Severity]
		if si != sj {
			return si < sj
		}
		if findings[i].Check != findings[j].Check {
			return findings[i].Check < findings[j].Check
		}
		return findings[i].Domain < findings[j].Domain
	})
}
