package egress

import (
	"context"
	"net"
	"strings"
	"testing"
)

func TestTheDefaultDenyListBlocksTheHostAndTheMetadataService(t *testing.T) {
	deny, err := ParsePrefixes(DefaultDeny)
	if err != nil {
		t.Fatal(err)
	}
	check := denyDial(deny, "test")
	for _, addr := range []string{
		"127.0.0.1:8080", "[::1]:8080", "169.254.169.254:80", "[fd00:ec2::254]:80",
		"0.0.0.0:80", "[fe80::1%eth0]:80", "100.100.100.200:80", "168.63.129.16:80",
		// IPv4 written as IPv6 is still loopback.
		"[::ffff:127.0.0.1]:80",
		// So is IPv4 reached through NAT64 or 6to4.
		"[64:ff9b::a9fe:a9fe]:80", "[2002:7f00:1::1]:80",
	} {
		if check("tcp", addr, nil) == nil {
			t.Errorf("%s was not blocked", addr)
		}
	}
	for _, addr := range []string{"10.0.0.7:8000", "[2001:db8::1]:443", "8.8.8.8:443", "[64:ff9b::808:808]:443"} {
		if err := check("tcp", addr, nil); err != nil {
			t.Errorf("%s was blocked: %v", addr, err)
		}
	}
}

func TestParsing(t *testing.T) {
	if deny, err := ParsePrefixes("none"); err != nil || deny != nil {
		t.Errorf("none = %v, %v", deny, err)
	}
	if deny, err := ParsePrefixes(" 10.1.2.3 , 192.168.0.0/16,,2001:db8::/32"); err != nil || len(deny) != 3 {
		t.Errorf("a list = %v, %v", deny, err)
	}
	for _, s := range []string{"127.0.0.1/33", "localhost", "10.0.0.0/8,nope"} {
		if _, err := ParsePrefixes(s); err == nil {
			t.Errorf("%q was accepted", s)
		}
	}

	for s, want := range map[string]bool{"": true, "none": true, "all": false, "gitlab.internal": true} {
		if limit, _, err := ParsePrivateHosts(s); err != nil || limit != want {
			t.Errorf("%q: limit = %v, %v; want %v", s, limit, err, want)
		}
	}
	if _, hosts, _ := ParsePrivateHosts(" GitLab.internal, git.corp "); strings.Join(hosts, ",") != "gitlab.internal,git.corp" {
		t.Errorf("hosts = %v", hosts)
	}
	for _, s := range []string{"https://gitlab.internal", "gitlab.internal:443"} {
		if _, _, err := ParsePrivateHosts(s); err == nil {
			t.Errorf("%q was accepted", s)
		}
	}
}

func TestOnlyNamedHostsReachInside(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	_, port, _ := net.SplitHostPort(l.Addr().String())
	ctx := context.Background()
	dial := func(p Policy, host string) error {
		c, err := p.Dial()(ctx, "tcp", net.JoinHostPort(host, port))
		if err == nil {
			c.Close()
		}
		return err
	}

	// Loopback stands in for the inside of the network; Deny is left empty
	// so it is not blocked for being loopback.
	limited := Policy{LimitPrivate: true, PrivateHosts: []string{"localhost"}}
	if err := dial(limited, "127.0.0.1"); err == nil || !strings.Contains(err.Error(), "may only reach localhost") {
		t.Errorf("an address not named: %v", err)
	}
	if err := dial(limited, "localhost"); err != nil {
		t.Errorf("a named host: %v", err)
	}
	if err := dial(Policy{LimitPrivate: true}, "localhost"); err == nil {
		t.Error("none named, yet a host inside was reached")
	}
	if err := dial(Policy{}, "127.0.0.1"); err != nil {
		t.Errorf("the zero policy: %v", err)
	}
	deny, _ := ParsePrefixes(DefaultDeny)
	if err := dial(Policy{Deny: deny, LimitPrivate: true, PrivateHosts: []string{"localhost"}}, "localhost"); err == nil {
		t.Error("a named host reached a denied address")
	}
}

// An address inside the network stays inside through a translator.
func TestInsideThroughATranslatorIsInside(t *testing.T) {
	check := denyDial(insideNetwork, "test")
	for _, addr := range []string{"[64:ff9b::a00:7]:443", "[2002:c0a8:101::1]:443", "[64:ff9b:1::a00:7]:443"} {
		if check("tcp", addr, nil) == nil {
			t.Errorf("%s was not blocked", addr)
		}
	}
	if err := check("tcp", "[64:ff9b::808:808]:443", nil); err != nil {
		t.Errorf("a public address through NAT64 was blocked: %v", err)
	}
}
