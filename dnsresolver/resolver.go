// Package dnsresolver provides automatic DNS failover for vmflow outbound
// connections.
package dnsresolver

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

const (
	defaultQueryTimeout    = 2 * time.Second
	defaultFailureCooldown = 30 * time.Second
)

var builtInFallbackEndpoints = [...]string{
	"223.5.5.5:53",
	"119.29.29.29:53",
}

type sourceHealth struct {
	unhealthyUntil atomic.Int64
}

type lookupSource struct {
	name      string
	lookupIP  func(context.Context, string) ([]net.IPAddr, error)
	lookupTXT func(context.Context, string) ([]string, error)
	health    *sourceHealth
}

func (source lookupSource) available(now time.Time) bool {
	return source.health == nil || source.health.unhealthyUntil.Load() <= now.UnixNano()
}

func (source lookupSource) markHealthy() {
	if source.health != nil {
		source.health.unhealthyUntil.Store(0)
	}
}

func (source lookupSource) markUnhealthy(until time.Time) {
	if source.health != nil {
		source.health.unhealthyUntil.Store(until.UnixNano())
	}
}

// Resolver tries the host operating system policy first and then the built-in
// domestic fallback resolvers. Temporarily failed sources are retried after a
// short cooldown, so normal system DNS automatically becomes primary again.
type Resolver struct {
	sources         []lookupSource
	queryTimeout    time.Duration
	failureCooldown time.Duration
	now             func() time.Time
}

// New builds vmflow's zero-configuration DNS resolver.
func New() *Resolver {
	sources := make([]lookupSource, 0, 1+len(builtInFallbackEndpoints))
	sources = append(sources, sourceFromNetResolver("system", net.DefaultResolver))
	for _, endpoint := range builtInFallbackEndpoints {
		sources = append(sources, sourceFromNetResolver(endpoint, resolverForEndpoint(endpoint)))
	}
	return newResolver(sources, defaultQueryTimeout, defaultFailureCooldown, time.Now)
}

func newResolver(sources []lookupSource, queryTimeout, failureCooldown time.Duration, now func() time.Time) *Resolver {
	if queryTimeout <= 0 {
		queryTimeout = defaultQueryTimeout
	}
	if failureCooldown <= 0 {
		failureCooldown = defaultFailureCooldown
	}
	if now == nil {
		now = time.Now
	}
	owned := append([]lookupSource(nil), sources...)
	for index := range owned {
		if owned[index].health == nil {
			owned[index].health = &sourceHealth{}
		}
	}
	return &Resolver{
		sources:         owned,
		queryTimeout:    queryTimeout,
		failureCooldown: failureCooldown,
		now:             now,
	}
}

func resolverForEndpoint(endpoint string) *net.Resolver {
	return &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			dialer := net.Dialer{}
			return dialer.DialContext(ctx, network, endpoint)
		},
	}
}

func sourceFromNetResolver(name string, resolver *net.Resolver) lookupSource {
	return lookupSource{
		name:      name,
		lookupIP:  resolver.LookupIPAddr,
		lookupTXT: resolver.LookupTXT,
		health:    &sourceHealth{},
	}
}

// LookupIPAddr resolves host using automatic failover.
func (resolver *Resolver) LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error) {
	host = strings.TrimSpace(host)
	if host == "" {
		return nil, errors.New("missing DNS name")
	}
	if address, err := netip.ParseAddr(host); err == nil {
		return []net.IPAddr{netIPAddr(address)}, nil
	}
	return lookupWithFailover(resolver, ctx, host, func(source lookupSource, attemptCtx context.Context) ([]net.IPAddr, error) {
		return source.lookupIP(attemptCtx, host)
	})
}

// LookupHost resolves host and returns textual IP addresses.
func (resolver *Resolver) LookupHost(ctx context.Context, host string) ([]string, error) {
	addresses, err := resolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	result := make([]string, 0, len(addresses))
	seen := make(map[string]struct{}, len(addresses))
	for _, address := range addresses {
		value := address.String()
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result, nil
}

// LookupTXT resolves TXT records using automatic failover.
func (resolver *Resolver) LookupTXT(ctx context.Context, name string) ([]string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, errors.New("missing DNS name")
	}
	return lookupWithFailover(resolver, ctx, name, func(source lookupSource, attemptCtx context.Context) ([]string, error) {
		return source.lookupTXT(attemptCtx, name)
	})
}

func lookupWithFailover[T any](resolver *Resolver, ctx context.Context, name string, lookup func(lookupSource, context.Context) ([]T, error)) ([]T, error) {
	if resolver == nil || len(resolver.sources) == 0 {
		resolver = New()
	}
	if ctx == nil {
		ctx = context.Background()
	}

	var failures []error
	used := make([]string, 0, len(resolver.sources))
	eligible := 0
	for _, source := range resolver.sources {
		if !source.available(resolver.now()) {
			continue
		}
		eligible++
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		attemptCtx, cancel := context.WithTimeout(ctx, resolver.queryTimeout)
		values, err := lookup(source, attemptCtx)
		cancel()
		used = append(used, source.name)
		if err == nil && len(values) > 0 {
			source.markHealthy()
			return values, nil
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err == nil {
			err = errors.New("DNS response contained no records")
		}
		if dnsNameNotFound(err) {
			source.markHealthy()
			return nil, fmt.Errorf("lookup %s via %s: %w", name, source.name, err)
		}
		source.markUnhealthy(resolver.now().Add(resolver.failureCooldown))
		failures = append(failures, fmt.Errorf("%s: %w", source.name, err))
	}
	if eligible == 0 {
		return nil, fmt.Errorf("lookup %s failed: all DNS resolvers are temporarily cooling down", name)
	}
	return nil, fmt.Errorf("lookup %s failed via %s: %w", name, strings.Join(used, ", "), joinFailures(failures))
}

func dnsNameNotFound(err error) bool {
	var dnsErr *net.DNSError
	return errors.As(err, &dnsErr) && dnsErr.IsNotFound
}

type failureList []error

func (failures failureList) Error() string {
	messages := make([]string, 0, len(failures))
	for _, err := range failures {
		if err != nil {
			messages = append(messages, err.Error())
		}
	}
	return strings.Join(messages, "; ")
}

func (failures failureList) Unwrap() []error { return []error(failures) }

func joinFailures(failures []error) error { return failureList(failures) }

// ResolveTCPAddr resolves a numeric-port TCP endpoint with automatic failover.
func (resolver *Resolver) ResolveTCPAddr(ctx context.Context, network, address string) (*net.TCPAddr, error) {
	host, port, err := splitEndpoint(address)
	if err != nil {
		return nil, err
	}
	ip, err := resolver.firstIP(ctx, network, host)
	if err != nil {
		return nil, err
	}
	return &net.TCPAddr{IP: ip.IP, Port: port, Zone: ip.Zone}, nil
}

// ResolveUDPAddr resolves a numeric-port UDP endpoint with automatic failover.
func (resolver *Resolver) ResolveUDPAddr(ctx context.Context, network, address string) (*net.UDPAddr, error) {
	host, port, err := splitEndpoint(address)
	if err != nil {
		return nil, err
	}
	ip, err := resolver.firstIP(ctx, network, host)
	if err != nil {
		return nil, err
	}
	return &net.UDPAddr{IP: ip.IP, Port: port, Zone: ip.Zone}, nil
}

func splitEndpoint(address string) (string, int, error) {
	host, portText, err := net.SplitHostPort(strings.TrimSpace(address))
	if err != nil {
		return "", 0, err
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return "", 0, fmt.Errorf("port must be between 1 and 65535")
	}
	return strings.TrimSpace(host), port, nil
}

func (resolver *Resolver) firstIP(ctx context.Context, network, host string) (net.IPAddr, error) {
	addresses, err := resolver.LookupIPAddr(ctx, host)
	if err != nil {
		return net.IPAddr{}, err
	}
	for _, address := range addresses {
		if networkAcceptsIP(network, address.IP) {
			return address, nil
		}
	}
	return net.IPAddr{}, fmt.Errorf("lookup %s returned no address for network %s", host, network)
}

func networkAcceptsIP(network string, ip net.IP) bool {
	switch {
	case strings.HasSuffix(network, "4"):
		return ip.To4() != nil
	case strings.HasSuffix(network, "6"):
		return ip.To4() == nil
	default:
		return true
	}
}

func netIPAddr(address netip.Addr) net.IPAddr {
	return net.IPAddr{IP: net.IP(address.AsSlice()), Zone: address.Zone()}
}

// DialContext resolves the host with automatic failover, then dials the
// returned addresses within the dialer's overall timeout/deadline.
func (resolver *Resolver) DialContext(ctx context.Context, dialer *net.Dialer, network, address string) (net.Conn, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if dialer == nil {
		dialer = &net.Dialer{}
	}
	host, portText, err := net.SplitHostPort(strings.TrimSpace(address))
	if err != nil {
		return nil, err
	}
	if _, err := netip.ParseAddr(host); err == nil || host == "" {
		return dialer.DialContext(ctx, network, address)
	}

	dialCtx, cancel := dialContext(ctx, dialer)
	defer cancel()
	addresses, err := resolver.LookupIPAddr(dialCtx, host)
	if err != nil {
		return nil, err
	}

	directDialer := *dialer
	directDialer.Timeout = 0
	directDialer.Deadline = time.Time{}
	var failures []error
	for _, ip := range addresses {
		if !networkAcceptsIP(network, ip.IP) {
			continue
		}
		endpoint := net.JoinHostPort(ip.String(), portText)
		conn, dialErr := directDialer.DialContext(dialCtx, network, endpoint)
		if dialErr == nil {
			return conn, nil
		}
		failures = append(failures, fmt.Errorf("%s: %w", endpoint, dialErr))
	}
	if len(failures) == 0 {
		return nil, fmt.Errorf("lookup %s returned no address for network %s", host, network)
	}
	return nil, fmt.Errorf("dial %s: %w", address, joinFailures(failures))
}

func dialContext(ctx context.Context, dialer *net.Dialer) (context.Context, context.CancelFunc) {
	deadline := dialer.Deadline
	if dialer.Timeout > 0 {
		timeoutDeadline := time.Now().Add(dialer.Timeout)
		if deadline.IsZero() || timeoutDeadline.Before(deadline) {
			deadline = timeoutDeadline
		}
	}
	if deadline.IsZero() {
		return context.WithCancel(ctx)
	}
	return context.WithDeadline(ctx, deadline)
}
