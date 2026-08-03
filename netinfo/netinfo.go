// Package netinfo collects a read-only snapshot of host network interfaces.
package netinfo

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	gnet "github.com/shirou/gopsutil/v3/net"
)

const (
	WarningHostnameUnavailable   = "hostname_unavailable"
	WarningInterfacesUnavailable = "interfaces_unavailable"
	WarningAddressesUnavailable  = "addresses_unavailable"
	WarningCountersUnavailable   = "counters_unavailable"
)

// Address describes one IP address assigned to an interface.
type Address struct {
	CIDR   string `json:"cidr"`
	Family string `json:"family"`
	Scope  string `json:"scope"`
	Zone   string `json:"zone,omitempty"`
}

// Interface describes one host network interface and its cumulative counters.
type Interface struct {
	Index             int       `json:"index,omitempty"`
	Name              string    `json:"name"`
	Up                bool      `json:"up"`
	Running           bool      `json:"running"`
	Loopback          bool      `json:"loopback"`
	PointToPoint      bool      `json:"point_to_point,omitempty"`
	MTU               int       `json:"mtu,omitempty"`
	Addresses         []Address `json:"addresses"`
	CountersAvailable bool      `json:"counters_available"`
	RXBytes           uint64    `json:"rx_bytes,omitempty"`
	TXBytes           uint64    `json:"tx_bytes,omitempty"`
}

// Snapshot is a point-in-time view of the daemon host's network interfaces.
// Warnings contain stable codes; raw platform errors stay in daemon logs.
type Snapshot struct {
	Available     bool        `json:"available"`
	CollectedTime int64       `json:"collected_time"`
	Hostname      string      `json:"hostname,omitempty"`
	OS            string      `json:"os"`
	Arch          string      `json:"arch"`
	Interfaces    []Interface `json:"interfaces"`
	Warnings      []string    `json:"warnings,omitempty"`
}

type interfaceRecord struct {
	iface     net.Interface
	addresses []net.Addr
	addrErr   error
}

type sources struct {
	hostname   func() (string, error)
	interfaces func() ([]interfaceRecord, error)
	counters   func(context.Context) ([]gnet.IOCountersStat, error)
	now        func() time.Time
	goos       string
	goarch     string
}

// Collect reads one host network snapshot. Partial data is returned together
// with an error when one platform source is unavailable.
func Collect(ctx context.Context) (Snapshot, error) {
	return collect(ctx, sources{
		hostname:   netHostname,
		interfaces: readInterfaces,
		counters: func(ctx context.Context) ([]gnet.IOCountersStat, error) {
			return gnet.IOCountersWithContext(ctx, true)
		},
		now:    time.Now,
		goos:   runtime.GOOS,
		goarch: runtime.GOARCH,
	})
}

func collect(ctx context.Context, src sources) (Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return Snapshot{}, err
	}
	snapshot := Snapshot{
		CollectedTime: src.now().Unix(),
		OS:            src.goos,
		Arch:          src.goarch,
		Interfaces:    make([]Interface, 0),
	}
	var failures []error

	if hostname, err := src.hostname(); err != nil {
		snapshot.Warnings = appendWarning(snapshot.Warnings, WarningHostnameUnavailable)
		failures = append(failures, fmt.Errorf("read hostname: %w", err))
	} else {
		snapshot.Hostname = strings.TrimSpace(hostname)
	}

	records, interfacesErr := src.interfaces()
	if interfacesErr != nil {
		snapshot.Warnings = appendWarning(snapshot.Warnings, WarningInterfacesUnavailable)
		failures = append(failures, fmt.Errorf("read network interfaces: %w", interfacesErr))
	}

	counters, countersErr := src.counters(ctx)
	if countersErr != nil {
		snapshot.Warnings = appendWarning(snapshot.Warnings, WarningCountersUnavailable)
		failures = append(failures, fmt.Errorf("read network counters: %w", countersErr))
	}
	countersByName := make(map[string]gnet.IOCountersStat, len(counters))
	for _, counter := range counters {
		name := strings.TrimSpace(counter.Name)
		if name != "" {
			countersByName[name] = counter
		}
	}

	seen := make(map[string]struct{}, len(records))
	for _, record := range records {
		name := strings.TrimSpace(record.iface.Name)
		if name == "" {
			continue
		}
		seen[name] = struct{}{}
		item := Interface{
			Index:        record.iface.Index,
			Name:         name,
			Up:           record.iface.Flags&net.FlagUp != 0,
			Running:      record.iface.Flags&net.FlagRunning != 0,
			Loopback:     record.iface.Flags&net.FlagLoopback != 0,
			PointToPoint: record.iface.Flags&net.FlagPointToPoint != 0,
			MTU:          record.iface.MTU,
			Addresses:    make([]Address, 0, len(record.addresses)),
		}
		if record.addrErr != nil {
			snapshot.Warnings = appendWarning(snapshot.Warnings, WarningAddressesUnavailable)
			failures = append(failures, fmt.Errorf("read addresses for interface %s: %w", name, record.addrErr))
		}
		for _, raw := range record.addresses {
			if address, ok := normalizeAddress(raw.String()); ok {
				item.Addresses = append(item.Addresses, address)
			}
		}
		sortAddresses(item.Addresses)
		if counter, ok := countersByName[name]; ok {
			item.CountersAvailable = true
			item.RXBytes = counter.BytesRecv
			item.TXBytes = counter.BytesSent
		}
		snapshot.Interfaces = append(snapshot.Interfaces, item)
	}

	// Counter-only entries keep the endpoint useful if interface metadata is
	// unavailable on a platform, while clearly reporting the partial result.
	for name, counter := range countersByName {
		if _, ok := seen[name]; ok {
			continue
		}
		snapshot.Interfaces = append(snapshot.Interfaces, Interface{
			Name:              name,
			Loopback:          likelyLoopbackName(name),
			Addresses:         make([]Address, 0),
			CountersAvailable: true,
			RXBytes:           counter.BytesRecv,
			TXBytes:           counter.BytesSent,
		})
	}

	sortInterfaces(snapshot.Interfaces)
	sort.Strings(snapshot.Warnings)
	snapshot.Available = len(snapshot.Interfaces) > 0
	return snapshot, errors.Join(failures...)
}

func netHostname() (string, error) {
	return os.Hostname()
}

func readInterfaces() ([]interfaceRecord, error) {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	records := make([]interfaceRecord, 0, len(interfaces))
	for _, iface := range interfaces {
		addresses, addrErr := iface.Addrs()
		records = append(records, interfaceRecord{iface: iface, addresses: addresses, addrErr: addrErr})
	}
	return records, nil
}

func normalizeAddress(raw string) (Address, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return Address{}, false
	}
	host, prefix, hasPrefix := strings.Cut(raw, "/")
	zone := ""
	if before, after, ok := strings.Cut(host, "%"); ok {
		host, zone = before, after
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return Address{}, false
	}
	family, bits := "ipv6", 128
	if ip4 := ip.To4(); ip4 != nil {
		ip = ip4
		family, bits = "ipv4", 32
	}
	prefixBits := bits
	if hasPrefix {
		parsed, err := strconv.Atoi(prefix)
		if err != nil || parsed < 0 || parsed > bits {
			return Address{}, false
		}
		prefixBits = parsed
	}
	return Address{
		CIDR:   fmt.Sprintf("%s/%d", ip.String(), prefixBits),
		Family: family,
		Scope:  addressScope(ip),
		Zone:   zone,
	}, true
}

func addressScope(ip net.IP) string {
	switch {
	case ip.IsUnspecified():
		return "unspecified"
	case ip.IsLoopback():
		return "loopback"
	case ip.IsLinkLocalUnicast():
		return "link_local"
	case ip.IsPrivate():
		return "private"
	case ip.IsMulticast():
		return "multicast"
	case ip.IsGlobalUnicast():
		return "global_unicast"
	default:
		return "other"
	}
}

func sortAddresses(addresses []Address) {
	sort.SliceStable(addresses, func(i, j int) bool {
		if addresses[i].Family != addresses[j].Family {
			return addresses[i].Family == "ipv4"
		}
		left, right := scopePriority(addresses[i].Scope), scopePriority(addresses[j].Scope)
		if left != right {
			return left < right
		}
		return addresses[i].CIDR < addresses[j].CIDR
	})
}

func scopePriority(scope string) int {
	switch scope {
	case "global_unicast":
		return 0
	case "private":
		return 1
	case "link_local":
		return 2
	case "loopback":
		return 3
	default:
		return 4
	}
}

func sortInterfaces(interfaces []Interface) {
	sort.SliceStable(interfaces, func(i, j int) bool {
		left, right := interfaces[i], interfaces[j]
		if left.Up != right.Up {
			return left.Up
		}
		if left.Loopback != right.Loopback {
			return !left.Loopback
		}
		if (len(left.Addresses) > 0) != (len(right.Addresses) > 0) {
			return len(left.Addresses) > 0
		}
		if left.Index > 0 && right.Index > 0 && left.Index != right.Index {
			return left.Index < right.Index
		}
		return strings.ToLower(left.Name) < strings.ToLower(right.Name)
	})
}

func likelyLoopbackName(name string) bool {
	name = strings.ToLower(strings.TrimSpace(name))
	return name == "lo" || strings.HasPrefix(name, "lo0") || strings.Contains(name, "loopback")
}

func appendWarning(warnings []string, warning string) []string {
	for _, existing := range warnings {
		if existing == warning {
			return warnings
		}
	}
	return append(warnings, warning)
}
