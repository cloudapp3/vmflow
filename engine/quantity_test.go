package engine

import (
	"encoding/json"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestHumanQuantitiesParseAndRender(t *testing.T) {
	tests := []struct {
		input string
		want  int64
		rate  bool
	}{
		{input: "100 Mbps", want: 12_500_000, rate: true},
		{input: "10 MiB/s", want: 10 * 1024 * 1024, rate: true},
		{input: "1.5 TB", want: 1_500_000_000_000},
		{input: "100 GiB", want: 100 * 1024 * 1024 * 1024},
	}
	for _, test := range tests {
		t.Run(test.input, func(t *testing.T) {
			if test.rate {
				var value ByteRate
				if err := value.UnmarshalText([]byte(test.input)); err != nil {
					t.Fatal(err)
				}
				if int64(value) != test.want {
					t.Fatalf("value = %d, want %d", value, test.want)
				}
				return
			}
			var value ByteSize
			if err := value.UnmarshalText([]byte(test.input)); err != nil {
				t.Fatal(err)
			}
			if int64(value) != test.want {
				t.Fatalf("value = %d, want %d", value, test.want)
			}
		})
	}
	if got := ByteRate(12_500_000).String(); got != "100 Mbps" {
		t.Fatalf("rate string = %q", got)
	}
	if got := ByteSize(100 * 1024 * 1024 * 1024).String(); got != "100 GiB" {
		t.Fatalf("size string = %q", got)
	}
	if got := ByteRate(1 << 20).String(); got != "1 MiB/s" {
		t.Fatalf("binary rate string = %q", got)
	}
	if got := ByteSize(1 << 20).String(); got != "1 MiB" {
		t.Fatalf("binary size string = %q", got)
	}
}

func TestByteRateFallbackRemainsParseable(t *testing.T) {
	const original = ByteRate(10_000_001)
	encoded := original.String()
	if encoded != "10000001 B/s" {
		t.Fatalf("fallback rate string = %q", encoded)
	}
	var decoded ByteRate
	if err := decoded.UnmarshalText([]byte(encoded)); err != nil {
		t.Fatal(err)
	}
	if decoded != original {
		t.Fatalf("fallback round trip = %d, want %d", decoded, original)
	}
}

func TestHumanQuantitiesRejectAmbiguousUnits(t *testing.T) {
	for _, input := range []string{"100M", "100mb", "1.25 bps", "-1 GB"} {
		var value ByteRate
		if err := value.UnmarshalText([]byte(input)); err == nil {
			t.Fatalf("ByteRate(%q) succeeded", input)
		}
	}
}

func TestRuleHumanTrafficConfigYAMLAndJSON(t *testing.T) {
	raw := `
rule_id: limited
name: limited
protocol: tcp
listen_port: 2201
target_addr: 127.0.0.1
target_port: 22
enabled: true
bandwidth_limit:
  rate: 100 Mbps
traffic_quota:
  daily: 100 GB
  monthly: 1 TiB
  timezone: Asia/Shanghai
`
	var rule Rule
	if err := yaml.Unmarshal([]byte(raw), &rule); err != nil {
		t.Fatal(err)
	}
	rule = rule.Standardize()
	if err := rule.Validate(); err != nil {
		t.Fatal(err)
	}
	if got := rule.Bandwidth.UploadBytesPerSecond(); got != 12_500_000 {
		t.Fatalf("upload limit = %d", got)
	}
	if got := int64(rule.TrafficQuota.Daily); got != 100_000_000_000 {
		t.Fatalf("daily quota = %d", got)
	}
	encoded, err := json.Marshal(rule)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"rate":"100 Mbps"`, `"daily":"100 GB"`, `"action":"block"`} {
		if !strings.Contains(string(encoded), want) {
			t.Fatalf("JSON %s missing %s", encoded, want)
		}
	}
}

func TestRuleRejectsMixedBandwidthSyntaxAndInvalidQuota(t *testing.T) {
	base := Rule{RuleID: "r", Name: "r", Protocol: ProtocolTCP, ListenPort: 1, TargetAddr: "127.0.0.1", TargetPort: 2, Enabled: true}
	mixed := base
	mixed.Bandwidth = &BandwidthLimit{Rate: 1, Upload: 1}
	if err := mixed.Validate(); err == nil || !strings.Contains(err.Error(), "cannot be combined") {
		t.Fatalf("mixed bandwidth error = %v", err)
	}
	invalid := base.Standardize()
	invalid.TrafficQuota = &TrafficQuota{Daily: 1, Timezone: "Not/AZone", Action: "block"}
	if err := invalid.Validate(); err == nil || !strings.Contains(err.Error(), "timezone") {
		t.Fatalf("timezone error = %v", err)
	}
}
