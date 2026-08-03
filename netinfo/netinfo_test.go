package netinfo

import (
	"context"
	"errors"
	"net"
	"slices"
	"testing"
	"time"

	gnet "github.com/shirou/gopsutil/v3/net"
)

type testAddr string

func (address testAddr) Network() string { return "ip" }
func (address testAddr) String() string  { return string(address) }

func TestCollectBuildsSortedInterfaceSnapshot(t *testing.T) {
	snapshot, err := collect(t.Context(), sources{
		hostname: func() (string, error) { return " vm-01 ", nil },
		interfaces: func() ([]interfaceRecord, error) {
			return []interfaceRecord{
				{
					iface:     net.Interface{Index: 2, Name: "lo", MTU: 65536, Flags: net.FlagUp | net.FlagRunning | net.FlagLoopback},
					addresses: []net.Addr{testAddr("127.0.0.1/8"), testAddr("::1/128")},
				},
				{
					iface: net.Interface{Index: 1, Name: "eth0", MTU: 1500, Flags: net.FlagUp | net.FlagRunning},
					addresses: []net.Addr{
						testAddr("fe80::1%eth0/64"),
						testAddr("203.0.113.10/24"),
						testAddr("10.0.0.4/24"),
					},
				},
			}, nil
		},
		counters: func(context.Context) ([]gnet.IOCountersStat, error) {
			return []gnet.IOCountersStat{
				{Name: "lo", BytesRecv: 10, BytesSent: 20},
				{Name: "eth0", BytesRecv: 1_024, BytesSent: 2_048},
			}, nil
		},
		now:    func() time.Time { return time.Unix(1_700_000_000, 0) },
		goos:   "linux",
		goarch: "amd64",
	})
	if err != nil {
		t.Fatalf("collect() error = %v", err)
	}
	if !snapshot.Available || snapshot.Hostname != "vm-01" || snapshot.CollectedTime != 1_700_000_000 {
		t.Fatalf("snapshot metadata = %+v", snapshot)
	}
	if len(snapshot.Interfaces) != 2 || snapshot.Interfaces[0].Name != "eth0" || snapshot.Interfaces[1].Name != "lo" {
		t.Fatalf("interfaces = %+v", snapshot.Interfaces)
	}
	eth0 := snapshot.Interfaces[0]
	if !eth0.Up || !eth0.Running || eth0.Loopback || !eth0.CountersAvailable || eth0.RXBytes != 1_024 || eth0.TXBytes != 2_048 {
		t.Fatalf("eth0 = %+v", eth0)
	}
	wantAddresses := []Address{
		{CIDR: "203.0.113.10/24", Family: "ipv4", Scope: "global_unicast"},
		{CIDR: "10.0.0.4/24", Family: "ipv4", Scope: "private"},
		{CIDR: "fe80::1/64", Family: "ipv6", Scope: "link_local", Zone: "eth0"},
	}
	if !slices.Equal(eth0.Addresses, wantAddresses) {
		t.Fatalf("eth0 addresses = %+v, want %+v", eth0.Addresses, wantAddresses)
	}
}

func TestCollectReturnsPartialSnapshotAndStableWarnings(t *testing.T) {
	counterErr := errors.New("counters unavailable")
	snapshot, err := collect(t.Context(), sources{
		hostname: func() (string, error) { return "vm-01", nil },
		interfaces: func() ([]interfaceRecord, error) {
			return []interfaceRecord{{
				iface:   net.Interface{Index: 1, Name: "eth0", Flags: net.FlagUp},
				addrErr: errors.New("addresses unavailable"),
			}}, nil
		},
		counters: func(context.Context) ([]gnet.IOCountersStat, error) { return nil, counterErr },
		now:      time.Now,
		goos:     "linux",
		goarch:   "arm64",
	})
	if err == nil || !errors.Is(err, counterErr) {
		t.Fatalf("collect() error = %v, want wrapped counter error", err)
	}
	if !snapshot.Available || len(snapshot.Interfaces) != 1 || snapshot.Interfaces[0].CountersAvailable {
		t.Fatalf("partial snapshot = %+v", snapshot)
	}
	wantWarnings := []string{WarningAddressesUnavailable, WarningCountersUnavailable}
	if !slices.Equal(snapshot.Warnings, wantWarnings) {
		t.Fatalf("warnings = %v, want %v", snapshot.Warnings, wantWarnings)
	}
}

func TestCollectUsesCounterOnlyFallback(t *testing.T) {
	interfacesErr := errors.New("interfaces unavailable")
	snapshot, err := collect(t.Context(), sources{
		hostname:   func() (string, error) { return "host", nil },
		interfaces: func() ([]interfaceRecord, error) { return nil, interfacesErr },
		counters: func(context.Context) ([]gnet.IOCountersStat, error) {
			return []gnet.IOCountersStat{{Name: "Ethernet", BytesRecv: 5, BytesSent: 7}}, nil
		},
		now:    time.Now,
		goos:   "windows",
		goarch: "amd64",
	})
	if err == nil || !errors.Is(err, interfacesErr) {
		t.Fatalf("collect() error = %v, want wrapped interface error", err)
	}
	if !snapshot.Available || len(snapshot.Interfaces) != 1 {
		t.Fatalf("counter fallback snapshot = %+v", snapshot)
	}
	iface := snapshot.Interfaces[0]
	if iface.Name != "Ethernet" || !iface.CountersAvailable || iface.RXBytes != 5 || iface.TXBytes != 7 || iface.Addresses == nil {
		t.Fatalf("counter fallback interface = %+v", iface)
	}
}

func TestNormalizeAddressRejectsInvalidPrefixes(t *testing.T) {
	tests := []struct {
		raw    string
		want   Address
		wantOK bool
	}{
		{raw: "192.168.1.5", want: Address{CIDR: "192.168.1.5/32", Family: "ipv4", Scope: "private"}, wantOK: true},
		{raw: "2001:db8::1/64", want: Address{CIDR: "2001:db8::1/64", Family: "ipv6", Scope: "global_unicast"}, wantOK: true},
		{raw: "192.168.1.5/64"},
		{raw: "not-an-ip"},
	}
	for _, test := range tests {
		t.Run(test.raw, func(t *testing.T) {
			got, ok := normalizeAddress(test.raw)
			if ok != test.wantOK || got != test.want {
				t.Fatalf("normalizeAddress(%q) = (%+v, %v), want (%+v, %v)", test.raw, got, ok, test.want, test.wantOK)
			}
		})
	}
}
