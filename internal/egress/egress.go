// Package egress keeps the control plane's calls to connectors' services off
// its own network.
//
// An organisation's administrator sets some of the addresses called, such as
// a GitLab URL. Without a guard, keera-agents would reach anything its host
// can: the cloud metadata service, which hands out the host's credentials,
// or services inside the network.
//
// The check runs on the dialled address, after DNS, so a name that resolves
// to a blocked address is blocked too. Behind a proxy, the proxy must filter.
// It works as Keera Gateway's does, with the same settings' meaning.
package egress

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"syscall"
	"time"
)

// DefaultDeny is loopback, link-local, the unspecified address and the cloud
// metadata addresses (AWS IPv6, Alibaba, Azure WireServer).
const DefaultDeny = "127.0.0.0/8,::1/128,169.254.0.0/16,fe80::/10,0.0.0.0/8,::/128," +
	"fd00:ec2::254/128,100.100.100.200/32,168.63.129.16/32"

// insideNetwork are the addresses not on the public internet, including
// carrier-grade NAT, which Tailscale uses.
var insideNetwork = []netip.Prefix{
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("::1/128"),
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("::/128"),
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("fe80::/10"),
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("fc00::/7"),
	// NAT64 for one network only (RFC 8215), which reaches its inside.
	netip.MustParsePrefix("64:ff9b:1::/48"),
	netip.MustParsePrefix("224.0.0.0/4"),
	netip.MustParsePrefix("ff00::/8"),
}

// Policy is where connectors may connect. The zero Policy allows everything,
// as tests need.
type Policy struct {
	// Deny is the addresses never connected to.
	Deny []netip.Prefix
	// LimitPrivate keeps connectors from addresses inside the network,
	// except through the host names in PrivateHosts.
	LimitPrivate bool
	PrivateHosts []string
}

// ParsePrefixes reads a comma-separated list of addresses and prefixes, as in
// KEERA_AGENTS_UPSTREAM_DENY. "none" is an empty list.
func ParsePrefixes(s string) ([]netip.Prefix, error) {
	if strings.EqualFold(strings.TrimSpace(s), "none") {
		return nil, nil
	}
	var out []netip.Prefix
	for part := range strings.SplitSeq(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if !strings.Contains(part, "/") {
			addr, err := netip.ParseAddr(part)
			if err != nil {
				return nil, fmt.Errorf("%q is not an address or a prefix", part)
			}
			out = append(out, netip.PrefixFrom(addr, addr.BitLen()))
			continue
		}
		p, err := netip.ParsePrefix(part)
		if err != nil {
			return nil, fmt.Errorf("%q is not an address or a prefix", part)
		}
		out = append(out, p.Masked())
	}
	return out, nil
}

// ParsePrivateHosts reads KEERA_AGENTS_UPSTREAM_PRIVATE: the host names
// inside the network connectors may reach, or "all" or "none".
func ParsePrivateHosts(s string) (limit bool, hosts []string, err error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "all":
		return false, nil, nil
	case "none", "":
		return true, nil, nil
	}
	for part := range strings.SplitSeq(s, ",") {
		part = strings.ToLower(strings.TrimSpace(part))
		if part == "" {
			continue
		}
		if strings.ContainsAny(part, ":/") {
			return false, nil, fmt.Errorf("%q is not a host name; name the host alone, "+
				"such as gitlab.internal", part)
		}
		hosts = append(hosts, part)
	}
	return true, hosts, nil
}

const dialTimeout = 10 * time.Second

// Dial connects as p allows. The host names in PrivateHosts reach inside the
// network, and every other name is kept to public addresses. Deny applies
// to all. The name is checked before DNS, so a name an organisation
// controls cannot resolve into the network.
func (p Policy) Dial() func(ctx context.Context, network, addr string) (net.Conn, error) {
	open := newDialer(denyDial(p.Deny, "KEERA_AGENTS_UPSTREAM_DENY blocks it"))
	if !p.LimitPrivate {
		return open.DialContext
	}
	why := "connectors may not reach addresses inside this network"
	if len(p.PrivateHosts) > 0 {
		why = "inside this network, connectors may only reach " + strings.Join(p.PrivateHosts, ", ")
	}
	closed := newDialer(denyDial(append(slices.Clone(p.Deny), insideNetwork...),
		why+"; see KEERA_AGENTS_UPSTREAM_PRIVATE"))
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, _, err := net.SplitHostPort(addr)
		if err == nil && slices.Contains(p.PrivateHosts, strings.ToLower(host)) {
			return open.DialContext(ctx, network, addr)
		}
		return closed.DialContext(ctx, network, addr)
	}
}

// Transport is http.DefaultTransport's settings, connecting as p allows.
func (p Policy) Transport() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.DialContext = p.Dial()
	return t
}

func newDialer(deny func(string, string, syscall.RawConn) error) *net.Dialer {
	return &net.Dialer{Timeout: dialTimeout, KeepAlive: 30 * time.Second, Control: deny}
}

// denyDial is a net.Dialer Control that refuses an address in deny.
func denyDial(deny []netip.Prefix, why string) func(network, address string, _ syscall.RawConn) error {
	if len(deny) == 0 {
		return nil
	}
	return func(_, address string, _ syscall.RawConn) error {
		ap, err := netip.ParseAddrPort(address)
		if err != nil {
			return err
		}
		// A prefix never matches a zoned or IPv4-mapped address, so strip both.
		ip := ap.Addr().WithZone("").Unmap()
		for _, p := range deny {
			if p.Contains(ip) || p.Contains(embedded(ip)) {
				return fmt.Errorf("keera-agents does not connect to %s; %s", ip, why)
			}
		}
		return nil
	}
}

var (
	nat64     = netip.MustParsePrefix("64:ff9b::/96")
	sixToFour = netip.MustParsePrefix("2002::/16")
)

// embedded is the IPv4 address that an IPv6 one reaches through a
// translator: NAT64 (RFC 6052) or 6to4 (RFC 3056). Otherwise it is the
// zero Addr, which no prefix contains. 64:ff9b::a9fe:a9fe reaches
// 169.254.169.254 as surely as the address itself does.
func embedded(ip netip.Addr) netip.Addr {
	b := ip.As16()
	switch {
	case nat64.Contains(ip):
		return netip.AddrFrom4([4]byte(b[12:16]))
	case sixToFour.Contains(ip):
		return netip.AddrFrom4([4]byte(b[2:6]))
	}
	return netip.Addr{}
}
