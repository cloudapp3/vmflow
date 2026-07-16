package engine

import (
	"fmt"
	"net/netip"
	"strings"
	"testing"
)

func TestSourceIPMatcherModesAndFamilies(t *testing.T) {
	tests := []struct {
		name   string
		mode   SourceIPMode
		values []string
		addr   string
		allow  bool
	}{
		{name: "off", addr: "192.0.2.1", allow: true},
		{name: "allow exact", mode: SourceIPModeAllowlist, values: []string{"192.0.2.1"}, addr: "192.0.2.1", allow: true},
		{name: "allow exact miss", mode: SourceIPModeAllowlist, values: []string{"192.0.2.1"}, addr: "192.0.2.2"},
		{name: "allow cidr", mode: SourceIPModeAllowlist, values: []string{"192.0.2.0/24"}, addr: "192.0.2.200", allow: true},
		{name: "deny cidr", mode: SourceIPModeDenylist, values: []string{"192.0.2.0/24"}, addr: "192.0.2.200"},
		{name: "deny miss", mode: SourceIPModeDenylist, values: []string{"192.0.2.0/24"}, addr: "198.51.100.1", allow: true},
		{name: "allow ipv6", mode: SourceIPModeAllowlist, values: []string{"2001:db8::/32"}, addr: "2001:db8:1::1", allow: true},
		{name: "mapped ipv4", mode: SourceIPModeAllowlist, values: []string{"192.0.2.1"}, addr: "::ffff:192.0.2.1", allow: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			matcher, err := compileSourceIPMatcher(tt.mode, tt.values)
			if err != nil {
				t.Fatal(err)
			}
			if got := matcher.allows(netip.MustParseAddr(tt.addr)); got != tt.allow {
				t.Fatalf("allows(%s) = %v, want %v", tt.addr, got, tt.allow)
			}
		})
	}
}

func TestSourceIPPolicyValidation(t *testing.T) {
	tooMany := make([]string, MaxSourceIPsPerRule+1)
	for index := range tooMany {
		tooMany[index] = "192.0.2.1"
	}
	tests := []struct {
		name   string
		mode   SourceIPMode
		values []string
		want   string
	}{
		{name: "entries without mode", values: []string{"192.0.2.1"}, want: "requires source_ip_mode"},
		{name: "allowlist empty", mode: SourceIPModeAllowlist, want: "requires at least one"},
		{name: "denylist empty", mode: SourceIPModeDenylist, want: "requires at least one"},
		{name: "invalid mode", mode: "permit", values: []string{"192.0.2.1"}, want: "invalid source_ip_mode"},
		{name: "hostname", mode: SourceIPModeAllowlist, values: []string{"example.com"}, want: "literal IP address or CIDR"},
		{name: "empty entry", mode: SourceIPModeAllowlist, values: []string{""}, want: "empty source IP entry"},
		{name: "zone", mode: SourceIPModeAllowlist, values: []string{"fe80::1%eth0"}, want: "zones are not supported"},
		{name: "too many", mode: SourceIPModeAllowlist, values: tooMany, want: "exceeds maximum"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := compileSourceIPMatcher(tt.mode, tt.values)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want substring %q", err, tt.want)
			}
		})
	}
}

func TestParseSourceIPPrefixNormalizesMappedIPv4(t *testing.T) {
	prefix, err := ParseSourceIPPrefix("::ffff:192.0.2.129/120")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := prefix.String(), "192.0.2.0/24"; got != want {
		t.Fatalf("prefix = %q, want %q", got, want)
	}
}

func TestRuntimeEqualUsesSemanticSourceIPPolicy(t *testing.T) {
	base := Rule{SourceIPMode: SourceIPModeAllowlist, SourceIPs: []string{"192.0.2.0/24", "2001:db8::/32"}}
	reordered := base
	reordered.SourceIPs = []string{"2001:db8::/32", "192.0.2.1/24", "192.0.2.0/24"}
	if !base.RuntimeEqual(reordered) {
		t.Fatal("equivalent, reordered source policies should not restart a rule")
	}
	denied := base
	denied.SourceIPMode = SourceIPModeDenylist
	if base.RuntimeEqual(denied) {
		t.Fatal("allowlist and denylist must not be runtime-equal")
	}
	changed := base
	changed.SourceIPs = []string{"198.51.100.0/24"}
	if base.RuntimeEqual(changed) {
		t.Fatal("different source IP entries must restart a rule")
	}
	off := Rule{}
	explicitOff := Rule{SourceIPMode: SourceIPModeOff}
	if !off.RuntimeEqual(explicitOff) {
		t.Fatal("omitted and explicit off policies should be runtime-equal")
	}
}

func TestSourceIPMatcherHotPathDoesNotAllocate(t *testing.T) {
	values := make([]string, MaxSourceIPsPerRule)
	for index := range values {
		values[index] = fmt.Sprintf("10.%d.0.0/16", index)
	}
	matcher, err := compileSourceIPMatcher(SourceIPModeDenylist, values)
	if err != nil {
		t.Fatal(err)
	}
	addr := netip.MustParseAddr("192.0.2.1")
	if allocs := testing.AllocsPerRun(1000, func() { matcher.allows(addr) }); allocs != 0 {
		t.Fatalf("source IP matcher allocations = %.2f, want 0", allocs)
	}
}

func TestTCPUDPBuildsPolicyIntoBothRunners(t *testing.T) {
	manager := NewManager(nil)
	runners, err := manager.buildRunners(Rule{
		Protocol: ProtocolTCPUDP, SourceIPMode: SourceIPModeDenylist,
		SourceIPs: []string{"192.0.2.0/24"},
	})
	if err != nil || len(runners) != 2 {
		t.Fatalf("build runners = (%d, %v), want two", len(runners), err)
	}
	addr := netip.MustParseAddr("192.0.2.1")
	if runners[0].(*tcpRunner).ipMatcher.allows(addr) || runners[1].(*udpRunner).ipMatcher.allows(addr) {
		t.Fatal("tcp+udp policy was not installed into both runners")
	}
}

func BenchmarkSourceIPMatcher256CIDRs(b *testing.B) {
	values := make([]string, MaxSourceIPsPerRule)
	for index := range values {
		values[index] = fmt.Sprintf("10.%d.0.0/16", index)
	}
	matcher, err := compileSourceIPMatcher(SourceIPModeDenylist, values)
	if err != nil {
		b.Fatal(err)
	}
	addr := netip.MustParseAddr("192.0.2.1")
	b.ReportAllocs()
	for b.Loop() {
		matcher.allows(addr)
	}
}

func TestRuleStandardizeDeepCopiesSourceIPs(t *testing.T) {
	original := Rule{SourceIPs: []string{" 192.0.2.1 "}}
	standardized := original.Standardize()
	standardized.SourceIPs[0] = "198.51.100.1"
	if original.SourceIPs[0] != " 192.0.2.1 " {
		t.Fatalf("standardize mutated input slice: %+v", original.SourceIPs)
	}
}
