package main

import (
	"context"
	"net"
	"testing"
	"time"

	gvdns "github.com/containers/gvisor-tap-vsock/pkg/services/dns"
	"github.com/miekg/dns"
)

// Answers nothing, so a query that reaches it proves the zone did not match.
type deadUpstream struct{}

func (deadUpstream) LookupIPAddr(context.Context, string) ([]net.IPAddr, error) {
	return nil, &net.DNSError{Err: "no upstream in test", IsNotFound: true}
}
func (deadUpstream) LookupCNAME(context.Context, string) (string, error) { return "", nil }
func (deadUpstream) LookupMX(context.Context, string) ([]*net.MX, error) { return nil, nil }
func (deadUpstream) LookupNS(context.Context, string) ([]*net.NS, error) { return nil, nil }
func (deadUpstream) LookupSRV(context.Context, string, string, string) (string, []*net.SRV, error) {
	return "", nil, nil
}
func (deadUpstream) LookupTXT(context.Context, string) ([]string, error) { return nil, nil }

// Serves the zones wildcardZones builds on a local port, the way the gateway
// serves them to the guest, and returns its address.
func serveZones(t *testing.T, domains []string) string {
	t.Helper()
	udpConn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	tcpLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv, err := gvdns.NewWithUpstreamResolver(udpConn, tcpLn, wildcardZones(domains), deadUpstream{})
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve()    //nolint:errcheck
	go srv.ServeTCP() //nolint:errcheck
	t.Cleanup(func() {
		udpConn.Close()
		tcpLn.Close()
	})
	return udpConn.LocalAddr().String()
}

func queryA(t *testing.T, addr, name string) *dns.Msg {
	t.Helper()
	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn(name), dns.TypeA)
	c := &dns.Client{Timeout: 5 * time.Second}
	resp, _, err := c.Exchange(m, addr)
	if err != nil {
		t.Fatalf("query %s: %v", name, err)
	}
	return resp
}

// The contract the guest depends on: any depth of subdomain resolves to the
// guest's own loopback, and a domain outside the zone is left to the upstream.
func TestWildcardZoneResolvesSubdomains(t *testing.T) {
	addr := serveZones(t, []string{"sprout.localhost", "dev.test."})

	for _, name := range []string{
		"feat-login.sprout.localhost",
		"api.feat-login.sprout.localhost",
		"anything.dev.test",
	} {
		resp := queryA(t, addr, name)
		if len(resp.Answer) != 1 {
			t.Fatalf("%s: got %d answers, want 1", name, len(resp.Answer))
		}
		a, ok := resp.Answer[0].(*dns.A)
		if !ok {
			t.Fatalf("%s: answer is %T, want *dns.A", name, resp.Answer[0])
		}
		if !a.A.Equal(net.IPv4(127, 0, 0, 1)) {
			t.Fatalf("%s resolved to %s, want 127.0.0.1", name, a.A)
		}
	}

	if resp := queryA(t, addr, "example.com"); len(resp.Answer) != 0 {
		t.Fatalf("example.com was answered locally: %v", resp.Answer)
	}
}

// A trailing dot in the option value must not produce "sprout.localhost..",
// which would match nothing.
func TestWildcardZoneNormalizesTrailingDot(t *testing.T) {
	for _, domain := range []string{"sprout.localhost", "sprout.localhost."} {
		zones := wildcardZones([]string{domain})
		if len(zones) != 1 {
			t.Fatalf("%q: got %d zones, want 1", domain, len(zones))
		}
		if zones[0].Name != "sprout.localhost." {
			t.Fatalf("%q became zone %q, want %q", domain, zones[0].Name, "sprout.localhost.")
		}
	}
}

// No domains means no zones, so every name reaches the upstream resolver.
func TestWildcardZonesEmpty(t *testing.T) {
	if zones := wildcardZones(nil); len(zones) != 0 {
		t.Fatalf("got %d zones for no domains, want none", len(zones))
	}
}
