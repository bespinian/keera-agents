package config

import (
	"strings"
	"testing"
	"time"
)

func base() map[string]string {
	return map[string]string{
		"KEERA_AGENTS_GATEWAY_URL":  "http://localhost:8080",
		"KEERA_AGENTS_SERVICE_KEY":  strings.Repeat("s", 32),
		"KEERA_AGENTS_DATABASE_URL": "postgres://x",
		"KEERA_AGENTS_SECRET_KEY":   "0123456789abcdef",
	}
}

func loadWith(m map[string]string) (Config, error) {
	return load(func(k string) string { return m[k] })
}

func TestDefaults(t *testing.T) {
	c, err := loadWith(base())
	if err != nil {
		t.Fatal(err)
	}
	if c.PublicURL != "http://127.0.0.1:8070" || c.Runtime.URL != c.PublicURL ||
		c.GatewayInternalURL != c.GatewayURL || c.Runtime.GatewayURL != c.GatewayURL {
		t.Fatalf("addresses: %+v", c)
	}
	if c.Runtime.IdleSuspend != 15*time.Minute || c.TaskTimeout != 30*time.Minute || c.SecureCookies {
		t.Fatalf("defaults: %+v", c)
	}
}

func TestRefusals(t *testing.T) {
	for name, edit := range map[string]func(map[string]string){
		"no gateway":    func(m map[string]string) { delete(m, "KEERA_AGENTS_GATEWAY_URL") },
		"gateway path":  func(m map[string]string) { m["KEERA_AGENTS_GATEWAY_URL"] = "http://x/y" },
		"short service": func(m map[string]string) { m["KEERA_AGENTS_SERVICE_KEY"] = "short" },
		"no database":   func(m map[string]string) { delete(m, "KEERA_AGENTS_DATABASE_URL") },
		"bad driver":    func(m map[string]string) { m["KEERA_AGENTS_DRIVER"] = "docker" },
		"tiny idle":     func(m map[string]string) { m["KEERA_AGENTS_IDLE_SUSPEND"] = "5s" },
		"no timeout":    func(m map[string]string) { m["KEERA_AGENTS_TASK_TIMEOUT"] = "0" },
		"bad userns":    func(m map[string]string) { m["KEERA_AGENTS_USER_NAMESPACES"] = "maybe" },
		"bad deny":      func(m map[string]string) { m["KEERA_AGENTS_UPSTREAM_DENY"] = "localhost" },
		"bad private":   func(m map[string]string) { m["KEERA_AGENTS_UPSTREAM_PRIVATE"] = "https://git.corp" },
		"short metrics": func(m map[string]string) { m["KEERA_AGENTS_METRICS_TOKEN"] = "short" },
		"metrics is service key": func(m map[string]string) {
			m["KEERA_AGENTS_METRICS_TOKEN"] = m["KEERA_AGENTS_SERVICE_KEY"]
		},
		"metrics is secret key": func(m map[string]string) {
			m["KEERA_AGENTS_METRICS_TOKEN"] = m["KEERA_AGENTS_SECRET_KEY"]
		},
	} {
		m := base()
		edit(m)
		if _, err := loadWith(m); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestIdleSuspendOff(t *testing.T) {
	m := base()
	m["KEERA_AGENTS_IDLE_SUSPEND"] = "0"
	m["KEERA_AGENTS_IMAGE_PULL_SECRETS"] = "a, b,"
	c, err := loadWith(m)
	if err != nil {
		t.Fatal(err)
	}
	if c.Runtime.IdleSuspend != 0 || len(c.Runtime.ImagePullSecrets) != 2 {
		t.Fatalf("%+v", c.Runtime)
	}
}

func TestUserNamespaces(t *testing.T) {
	c, err := loadWith(base())
	if err != nil || c.Runtime.UserNamespaces {
		t.Fatalf("off by default: %v, %+v", err, c.Runtime)
	}
	m := base()
	m["KEERA_AGENTS_USER_NAMESPACES"] = "true"
	if c, err = loadWith(m); err != nil || !c.Runtime.UserNamespaces {
		t.Fatalf("on: %v, %+v", err, c.Runtime)
	}
}

func TestEgress(t *testing.T) {
	c, err := loadWith(base())
	if err != nil || len(c.Egress.Deny) == 0 || !c.Egress.LimitPrivate || len(c.Egress.PrivateHosts) != 0 {
		t.Fatalf("by default, the network is closed: %v, %+v", err, c.Egress)
	}
	m := base()
	m["KEERA_AGENTS_UPSTREAM_PRIVATE"] = "gitlab.corp"
	if c, err = loadWith(m); err != nil || !c.Egress.LimitPrivate || len(c.Egress.PrivateHosts) != 1 {
		t.Fatalf("one host: %v, %+v", err, c.Egress)
	}
	m["KEERA_AGENTS_UPSTREAM_PRIVATE"] = "all"
	m["KEERA_AGENTS_UPSTREAM_DENY"] = "none"
	if c, err = loadWith(m); err != nil || c.Egress.LimitPrivate || c.Egress.Deny != nil {
		t.Fatalf("all open: %v, %+v", err, c.Egress)
	}
}

func TestMetricsToken(t *testing.T) {
	m := base()
	if c, err := loadWith(m); err != nil || c.MetricsToken != "" {
		t.Fatalf("off by default: %q, %v", c.MetricsToken, err)
	}
	m["KEERA_AGENTS_METRICS_TOKEN"] = strings.Repeat("m", 32)
	if c, err := loadWith(m); err != nil || c.MetricsToken != m["KEERA_AGENTS_METRICS_TOKEN"] {
		t.Fatalf("token: %q, %v", c.MetricsToken, err)
	}
}
