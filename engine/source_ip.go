package engine

import (
	"fmt"
	"net"
	"net/netip"
	"sort"
	"strings"
)

// SourceIPMode controls how a rule interprets its SourceIPs entries.
type SourceIPMode string

const (
	SourceIPModeOff       SourceIPMode = "off"
	SourceIPModeAllowlist SourceIPMode = "allowlist"
	SourceIPModeDenylist  SourceIPMode = "denylist"

	// MaxSourceIPsPerRule bounds the work performed for every new connection or
	// UDP datagram. Exact addresses use a map; CIDR prefixes are scanned.
	MaxSourceIPsPerRule = 256
)

type sourceIPMatcher struct {
	mode     SourceIPMode
	exact    map[netip.Addr]struct{}
	prefixes []netip.Prefix
	entries  []netip.Prefix
}

// ParseSourceIPPrefix parses one literal source IP or CIDR. Literal addresses
// are represented as /32 or /128 prefixes, and IPv4-mapped IPv6 values are
// normalized to IPv4 so socket-family representation cannot bypass a policy.
func ParseSourceIPPrefix(value string) (netip.Prefix, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return netip.Prefix{}, fmt.Errorf("empty source IP entry")
	}
	if addr, err := netip.ParseAddr(value); err == nil {
		if addr.Zone() != "" {
			return netip.Prefix{}, fmt.Errorf("source IP zones are not supported: %q", value)
		}
		addr = addr.Unmap()
		return netip.PrefixFrom(addr, addr.BitLen()), nil
	}

	prefix, err := netip.ParsePrefix(value)
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("must be a literal IP address or CIDR: %q", value)
	}
	addr := prefix.Addr()
	if addr.Zone() != "" {
		return netip.Prefix{}, fmt.Errorf("source IP zones are not supported: %q", value)
	}
	bits := prefix.Bits()
	if addr.Is4In6() {
		if bits < 96 {
			return netip.Prefix{}, fmt.Errorf("IPv4-mapped IPv6 prefix must be /96 or narrower: %q", value)
		}
		addr = addr.Unmap()
		bits -= 96
	}
	return netip.PrefixFrom(addr, bits).Masked(), nil
}

func normalizeSourceIPMode(mode SourceIPMode) SourceIPMode {
	return SourceIPMode(strings.ToLower(strings.TrimSpace(string(mode))))
}

func compileSourceIPMatcher(mode SourceIPMode, values []string) (*sourceIPMatcher, error) {
	mode = normalizeSourceIPMode(mode)
	switch mode {
	case "", SourceIPModeOff:
		if len(values) != 0 {
			return nil, fmt.Errorf("source_ips requires source_ip_mode allowlist or denylist")
		}
		return nil, nil
	case SourceIPModeAllowlist, SourceIPModeDenylist:
		if len(values) == 0 {
			return nil, fmt.Errorf("source_ip_mode %s requires at least one source_ips entry", mode)
		}
	default:
		return nil, fmt.Errorf("invalid source_ip_mode: %s", mode)
	}
	if len(values) > MaxSourceIPsPerRule {
		return nil, fmt.Errorf("source_ips exceeds maximum of %d entries", MaxSourceIPsPerRule)
	}

	unique := make(map[netip.Prefix]struct{}, len(values))
	for index, value := range values {
		prefix, err := ParseSourceIPPrefix(value)
		if err != nil {
			return nil, fmt.Errorf("source_ips[%d]: %w", index, err)
		}
		unique[prefix] = struct{}{}
	}
	entries := make([]netip.Prefix, 0, len(unique))
	for prefix := range unique {
		entries = append(entries, prefix)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].String() < entries[j].String() })

	matcher := &sourceIPMatcher{mode: mode, entries: entries}
	for _, prefix := range entries {
		if prefix.Bits() == prefix.Addr().BitLen() {
			if matcher.exact == nil {
				matcher.exact = make(map[netip.Addr]struct{})
			}
			matcher.exact[prefix.Addr()] = struct{}{}
			continue
		}
		matcher.prefixes = append(matcher.prefixes, prefix)
	}
	return matcher, nil
}

func (matcher *sourceIPMatcher) allows(addr netip.Addr) bool {
	if matcher == nil {
		return true
	}
	if !addr.IsValid() {
		return false
	}
	addr = addr.Unmap().WithZone("")
	_, matched := matcher.exact[addr]
	if !matched {
		for _, prefix := range matcher.prefixes {
			if prefix.Contains(addr) {
				matched = true
				break
			}
		}
	}
	if matcher.mode == SourceIPModeAllowlist {
		return matched
	}
	return !matched
}

func sourceIPFromNetAddr(value net.Addr) (netip.Addr, bool) {
	switch addr := value.(type) {
	case *net.TCPAddr:
		parsed := addr.AddrPort().Addr()
		return parsed.Unmap().WithZone(""), parsed.IsValid()
	case *net.UDPAddr:
		parsed := addr.AddrPort().Addr()
		return parsed.Unmap().WithZone(""), parsed.IsValid()
	}
	if value == nil {
		return netip.Addr{}, false
	}
	addrPort, err := netip.ParseAddrPort(value.String())
	if err != nil {
		return netip.Addr{}, false
	}
	return addrPort.Addr().Unmap().WithZone(""), true
}

func sourceIPPoliciesEqual(left, right Rule) bool {
	leftMatcher, leftErr := compileSourceIPMatcher(left.SourceIPMode, left.SourceIPs)
	rightMatcher, rightErr := compileSourceIPMatcher(right.SourceIPMode, right.SourceIPs)
	if leftErr != nil || rightErr != nil {
		return false
	}
	if leftMatcher == nil || rightMatcher == nil {
		return leftMatcher == nil && rightMatcher == nil
	}
	if leftMatcher.mode != rightMatcher.mode || len(leftMatcher.entries) != len(rightMatcher.entries) {
		return false
	}
	for index := range leftMatcher.entries {
		if leftMatcher.entries[index] != rightMatcher.entries[index] {
			return false
		}
	}
	return true
}
