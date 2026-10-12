package postgres

import (
	"context"
	"maps"
	"strings"
	"testing"

	"github.com/bespinian/keera-agents/internal/channels"
)

func conn(config map[string]string) channels.Connection {
	c := map[string]string{"host": "db.example.com", "database": "shop", "user": "agents"}
	maps.Copy(c, config)
	return channels.Connection{Kind: channels.Postgres, Config: c, Secrets: map[string]string{"password": "p@ss/word"}}
}

func TestConfig(t *testing.T) {
	a := New(channels.Options{})
	cfg, err := a.config(conn(nil), true)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Host != "db.example.com" || cfg.Port != 5432 || cfg.Password != "p@ss/word" || cfg.TLSConfig == nil ||
		cfg.TLSConfig.ServerName != "db.example.com" || cfg.RuntimeParams["default_transaction_read_only"] != "on" {
		t.Errorf("config = %+v", cfg.Config)
	}
	if hosts, _ := cfg.LookupFunc(context.Background(), "db.example.com"); len(hosts) != 1 || hosts[0] != "db.example.com" {
		t.Errorf("the name was resolved before the egress guard saw it: %v", hosts)
	}
	if cfg, _ := a.config(conn(map[string]string{"sslmode": "disable", "port": "6432"}), false); cfg.TLSConfig != nil ||
		cfg.Port != 6432 || cfg.RuntimeParams["default_transaction_read_only"] != "" {
		t.Errorf("without TLS, writing: %+v", cfg.Config)
	}
	for name, bad := range map[string]map[string]string{
		"a host with a port":   {"host": "db:5432"},
		"a host with a path":   {"host": "db/x"},
		"a bad port":           {"port": "99999"},
		"TLS not verified":     {"sslmode": "require"},
		"a CA that is not PEM": {"ca_cert": "nope"},
		"no database":          {"database": ""},
	} {
		if _, err := a.config(conn(bad), true); err == nil {
			t.Errorf("%s passed", name)
		}
	}
	if _, err := a.config(conn(map[string]string{"host": "2001:db8::1"}), true); err != nil {
		t.Errorf("an IPv6 address: %v", err)
	}
}

func TestTarget(t *testing.T) {
	a := New(channels.Options{})
	if got, err := a.Target(context.Background(), conn(nil), "*"); err != nil || got != "*" {
		t.Errorf("Target(*) = %q, %v", got, err)
	}
	if _, err := a.Target(context.Background(), conn(nil), "public"); err == nil {
		t.Error("a schema passed, though it would narrow nothing")
	}
}

func TestCell(t *testing.T) {
	if got := cell(strings.Repeat("é", maxCell+5)); !strings.HasSuffix(got, "…[cut]") || len([]rune(got)) != maxCell+6 {
		t.Errorf("cell = %d characters", len([]rune(got)))
	}
}
