package dnsresolver

import (
	"context"
	"encoding/binary"
	"errors"
	"net"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestNewUsesSystemThenDomesticFallbacks(t *testing.T) {
	resolver := New()
	got := make([]string, 0, len(resolver.sources))
	for _, source := range resolver.sources {
		got = append(got, source.name)
	}
	want := []string{"system", "223.5.5.5:53", "119.29.29.29:53"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("resolver order = %#v, want %#v", got, want)
	}
}

func TestLookupUsesSystemBeforeFallback(t *testing.T) {
	var calls []string
	resolver := testResolver([]lookupSource{
		testSource("system", &calls, []net.IPAddr{{IP: net.ParseIP("192.0.2.10")}}, nil),
		testSource("fallback", &calls, nil, errors.New("must not be called")),
	}, time.Now)

	addresses, err := resolver.LookupIPAddr(context.Background(), "target.example")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := addresses[0].IP.String(), "192.0.2.10"; got != want {
		t.Fatalf("address = %q, want %q", got, want)
	}
	if want := []string{"system"}; !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls = %#v, want %#v", calls, want)
	}
}

func TestLookupFailsOverAfterTemporarySystemFailure(t *testing.T) {
	var calls []string
	resolver := testResolver([]lookupSource{
		testSource("system", &calls, nil, errors.New("network unreachable")),
		testSource("223.5.5.5:53", &calls, []net.IPAddr{{IP: net.ParseIP("198.51.100.7")}}, nil),
		testSource("119.29.29.29:53", &calls, nil, errors.New("must not be called")),
	}, time.Now)

	addresses, err := resolver.LookupIPAddr(context.Background(), "target.example")
	if err != nil {
		t.Fatal(err)
	}
	if got := addresses[0].IP.String(); got != "198.51.100.7" {
		t.Fatalf("address = %q", got)
	}
	want := []string{"system", "223.5.5.5:53"}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls = %#v, want %#v", calls, want)
	}
}

func TestLookupNXDOMAINDoesNotLeakToFallbacks(t *testing.T) {
	var calls []string
	resolver := testResolver([]lookupSource{
		testSource("system", &calls, nil, &net.DNSError{Err: "no such host", Name: "internal.example", IsNotFound: true}),
		testSource("fallback", &calls, []net.IPAddr{{IP: net.ParseIP("203.0.113.8")}}, nil),
	}, time.Now)

	_, err := resolver.LookupIPAddr(context.Background(), "internal.example")
	if err == nil || !strings.Contains(err.Error(), "no such host") {
		t.Fatalf("lookup error = %v", err)
	}
	if want := []string{"system"}; !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls = %#v, want %#v", calls, want)
	}
}

func TestFailedSourceCooldownAndAutomaticFailback(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	systemHealthy := false
	var calls []string
	system := lookupSource{
		name: "system",
		lookupIP: func(context.Context, string) ([]net.IPAddr, error) {
			calls = append(calls, "system")
			if !systemHealthy {
				return nil, errors.New("temporary failure")
			}
			return []net.IPAddr{{IP: net.ParseIP("192.0.2.30")}}, nil
		},
	}
	fallback := testSource("fallback", &calls, []net.IPAddr{{IP: net.ParseIP("192.0.2.31")}}, nil)
	resolver := testResolver([]lookupSource{system, fallback}, func() time.Time { return now })

	first, err := resolver.LookupIPAddr(context.Background(), "target.example")
	if err != nil || first[0].IP.String() != "192.0.2.31" {
		t.Fatalf("first lookup = %v, %v", first, err)
	}
	second, err := resolver.LookupIPAddr(context.Background(), "target.example")
	if err != nil || second[0].IP.String() != "192.0.2.31" {
		t.Fatalf("second lookup = %v, %v", second, err)
	}

	systemHealthy = true
	now = now.Add(defaultFailureCooldown + time.Second)
	third, err := resolver.LookupIPAddr(context.Background(), "target.example")
	if err != nil || third[0].IP.String() != "192.0.2.30" {
		t.Fatalf("third lookup = %v, %v", third, err)
	}
	want := []string{"system", "fallback", "fallback", "system"}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls = %#v, want %#v", calls, want)
	}
}

func TestLookupFailureReportsEveryResolverAndThenCoolsDown(t *testing.T) {
	var calls []string
	resolver := testResolver([]lookupSource{
		testSource("system", &calls, nil, errors.New("first failure")),
		testSource("fallback", &calls, nil, errors.New("second failure")),
	}, time.Now)
	_, err := resolver.LookupIPAddr(context.Background(), "target.example")
	if err == nil {
		t.Fatal("expected lookup failure")
	}
	for _, value := range []string{"system", "fallback", "first failure", "second failure"} {
		if !strings.Contains(err.Error(), value) {
			t.Fatalf("error %q does not contain %q", err, value)
		}
	}

	callCount := len(calls)
	_, err = resolver.LookupIPAddr(context.Background(), "target.example")
	if err == nil || !strings.Contains(err.Error(), "cooling down") {
		t.Fatalf("cooldown error = %v", err)
	}
	if len(calls) != callCount {
		t.Fatalf("cooling sources were queried: calls = %#v", calls)
	}
}

func TestResolveUDPAddrFiltersAddressFamily(t *testing.T) {
	resolver := testResolver([]lookupSource{{
		name: "system",
		lookupIP: func(context.Context, string) ([]net.IPAddr, error) {
			return []net.IPAddr{{IP: net.ParseIP("2001:db8::10")}, {IP: net.ParseIP("192.0.2.20")}}, nil
		},
	}}, time.Now)
	address, err := resolver.ResolveUDPAddr(context.Background(), "udp4", "target.example:53")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := address.String(), "192.0.2.20:53"; got != want {
		t.Fatalf("address = %q, want %q", got, want)
	}
}

func TestDialContextUsesResolvedAddress(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr == nil {
			accepted <- conn
		}
	}()

	resolver := testResolver([]lookupSource{{
		name: "system",
		lookupIP: func(context.Context, string) ([]net.IPAddr, error) {
			return []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}}, nil
		},
	}}, time.Now)
	_, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	conn, err := resolver.DialContext(context.Background(), &net.Dialer{Timeout: time.Second}, "tcp", net.JoinHostPort("target.example", port))
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
	select {
	case serverConn := <-accepted:
		serverConn.Close()
	case <-time.After(time.Second):
		t.Fatal("resolved connection was not accepted")
	}
}

func TestResolverFailoverWithRealDNSQueries(t *testing.T) {
	closed, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	unreachable := closed.LocalAddr().String()
	closed.Close()
	working := startTestDNSServer(t, net.ParseIP("192.0.2.44"))

	resolver := newResolver([]lookupSource{
		sourceFromNetResolver(unreachable, resolverForEndpoint(unreachable)),
		sourceFromNetResolver(working, resolverForEndpoint(working)),
	}, 100*time.Millisecond, time.Second, time.Now)
	addresses, err := resolver.LookupHost(context.Background(), "target.test")
	if err != nil {
		t.Fatal(err)
	}
	if !containsString(addresses, "192.0.2.44") {
		t.Fatalf("addresses = %#v, want 192.0.2.44", addresses)
	}
}

func testResolver(sources []lookupSource, now func() time.Time) *Resolver {
	return newResolver(sources, time.Second, defaultFailureCooldown, now)
}

func testSource(name string, calls *[]string, addresses []net.IPAddr, err error) lookupSource {
	return lookupSource{
		name: name,
		lookupIP: func(context.Context, string) ([]net.IPAddr, error) {
			*calls = append(*calls, name)
			return addresses, err
		},
	}
}

func startTestDNSServer(t *testing.T, ipv4 net.IP) string {
	t.Helper()
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	go func() {
		buffer := make([]byte, 512)
		for {
			n, client, readErr := conn.ReadFromUDP(buffer)
			if readErr != nil {
				return
			}
			response := testDNSResponse(buffer[:n], ipv4)
			if response != nil {
				_, _ = conn.WriteToUDP(response, client)
			}
		}
	}()
	return conn.LocalAddr().String()
}

func testDNSResponse(query []byte, ipv4 net.IP) []byte {
	if len(query) < 17 {
		return nil
	}
	questionEnd := 12
	for questionEnd < len(query) && query[questionEnd] != 0 {
		labelLength := int(query[questionEnd])
		questionEnd += labelLength + 1
	}
	questionEnd += 5
	if questionEnd > len(query) {
		return nil
	}
	queryType := binary.BigEndian.Uint16(query[questionEnd-4 : questionEnd-2])
	response := append([]byte(nil), query[:questionEnd]...)
	response[2], response[3] = 0x81, 0x80
	binary.BigEndian.PutUint16(response[4:6], 1)
	binary.BigEndian.PutUint16(response[6:8], 0)
	binary.BigEndian.PutUint16(response[8:10], 0)
	binary.BigEndian.PutUint16(response[10:12], 0)
	if queryType != 1 {
		return response
	}
	binary.BigEndian.PutUint16(response[6:8], 1)
	answer := []byte{
		0xc0, 0x0c,
		0x00, 0x01,
		0x00, 0x01,
		0x00, 0x00, 0x00, 0x3c,
		0x00, 0x04,
	}
	answer = append(answer, ipv4.To4()...)
	return append(response, answer...)
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
