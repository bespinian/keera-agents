// Package config reads keera-agents' settings from the environment.
package config

import (
	"errors"
	"fmt"
	"math"
	"net"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/bespinian/keera-agents/internal/egress"
)

// DefaultAddr is where the control plane listens unless KEERA_AGENTS_ADDR
// says otherwise.
const DefaultAddr = ":8070"

// Config is the control plane's configuration. The agent runtime inside a
// container reads its own few variables: see internal/protocol, and
// KEERA_AGENT_LOG_LEVEL in cmd/keera-agents.
type Config struct {
	// Addr serves the panel at /, its API at /api, sign-in at /auth, connector
	// webhooks at /hooks and the agents' runtime API at /runtime.
	Addr string

	// PublicURL is keera-agents' origin as a browser and the chat services
	// see it. It must match the gateway's KEERA_AGENTS_URL, since sign-in
	// returns to /auth/callback, and webhooks are registered under it.
	PublicURL string

	// GatewayURL is the gateway's origin as a browser sees it.
	GatewayURL string
	// GatewayInternalURL is how keera-agents reaches the gateway. Defaults to
	// GatewayURL.
	GatewayInternalURL string
	// ServiceKey is the gateway's KEERA_AGENTS_SERVICE_KEY. It mints and
	// revokes agents' keys, looks people up and reads what an agent may use,
	// and does nothing else.
	ServiceKey string

	// DatabaseURL is the Postgres keera-agents keeps its state in.
	DatabaseURL string

	// SecretKey seals cookies, agents' gateway keys and connection
	// credentials. Changing it signs everyone out and makes every stored
	// credential unreadable.
	SecretKey string

	SecureCookies bool

	// MetricsToken reads /metrics and nothing else. Empty turns /metrics off.
	MetricsToken string

	Runtime RuntimeConfig

	// TaskTimeout ends a task that has run this long.
	TaskTimeout time.Duration
	// WatchInterval is how often watched Drive and OneDrive folders are
	// checked for new files.
	WatchInterval time.Duration

	// Egress is where connectors may connect: never to loopback or the
	// cloud metadata service, and inside the network only to the hosts
	// named. See docs/install.md.
	Egress egress.Policy

	// Prices of machine time, in micro-CHF, billed to the gateway. All zero
	// is no billing. See docs/billing.md.
	PriceCPUHour       int64
	PriceMemoryGiBHour int64
	PriceDiskGiBMonth  int64

	LogLevel  string
	LogFormat string
}

// RuntimeConfig is where agents run.
type RuntimeConfig struct {
	// Driver is "podman", "kubernetes", or empty for no runtime: agents can
	// be set up but not started.
	Driver string
	// Image is the agent image. Its entrypoint must run `keera-agents agent`;
	// see the Containerfile's agent stage.
	Image string
	// URL is keera-agents as an agent container reaches it. Defaults to
	// PublicURL.
	URL string
	// GatewayURL is the gateway as an agent container reaches it. Defaults
	// to GatewayInternalURL.
	GatewayURL string
	// IdleSuspend suspends an agent with no task for this long. Zero never
	// suspends.
	IdleSuspend time.Duration
	// WorkspaceImage is the workspace image. Its entrypoint must run
	// `keera-agents workspace`; see the Containerfile's workspace stage.
	WorkspaceImage string
	// WorkspaceIdleSuspend suspends a workspace nobody is connected to for
	// this long. Zero never suspends.
	WorkspaceIdleSuspend time.Duration

	// Runtimes per isolation tier: a RuntimeClass on Kubernetes, an OCI
	// runtime on podman. An unmapped tier other than standard is refused.
	RuntimeStandard string
	RuntimeIsolated string
	RuntimeVM       string

	// Kubernetes only.
	Namespace        string
	StorageClass     string
	ServiceAccount   string
	ImagePullSecrets []string
	// UserNamespaces runs standard-tier pods with hostUsers: false.
	UserNamespaces bool

	// Limits on the class an agent may ask for; runtime.LimitsFrom checks
	// them.
	MinIsolation               string
	MaxCPU, MaxMemory, MaxDisk string

	// Podman only.
	PodmanBinary  string
	PodmanNetwork string
}

// Load reads the configuration from the environment.
func Load() (Config, error) { return load(os.Getenv) }

func load(getenv func(string) string) (Config, error) {
	env := func(key, def string) string {
		if v := strings.TrimSpace(getenv(key)); v != "" {
			return v
		}
		return def
	}
	originEnv := func(key, def string) (string, error) { return origin(key, env(key, def)) }
	durationEnv := func(key string, def, least time.Duration) (time.Duration, error) {
		v := env(key, "")
		if v == "" {
			return def, nil
		}
		d, err := time.ParseDuration(v)
		if err != nil || (d != 0 && d < least) {
			return 0, fmt.Errorf("%s must be a duration such as 15m, at least %s, or 0 for off, not %q",
				key, least, v)
		}
		return d, nil
	}
	boolEnv := func(key string, def bool) (bool, error) {
		v := env(key, "")
		if v == "" {
			return def, nil
		}
		b, err := strconv.ParseBool(v)
		if err != nil {
			return false, fmt.Errorf("%s must be true or false, not %q", key, v)
		}
		return b, nil
	}

	c := Config{
		Addr:         env("KEERA_AGENTS_ADDR", DefaultAddr),
		ServiceKey:   env("KEERA_AGENTS_SERVICE_KEY", ""),
		DatabaseURL:  env("KEERA_AGENTS_DATABASE_URL", ""),
		SecretKey:    env("KEERA_AGENTS_SECRET_KEY", ""),
		MetricsToken: env("KEERA_AGENTS_METRICS_TOKEN", ""),
		LogLevel:     env("KEERA_AGENTS_LOG_LEVEL", "info"),
		LogFormat:    env("KEERA_AGENTS_LOG_FORMAT", "text"),
		Runtime: RuntimeConfig{
			Driver:          env("KEERA_AGENTS_DRIVER", ""),
			Image:           env("KEERA_AGENTS_IMAGE", "localhost/keera-agent:latest"),
			WorkspaceImage:  env("KEERA_AGENTS_WORKSPACE_IMAGE", "localhost/keera-workspace:latest"),
			RuntimeStandard: env("KEERA_AGENTS_RUNTIME_STANDARD", ""),
			RuntimeIsolated: env("KEERA_AGENTS_RUNTIME_ISOLATED", ""),
			RuntimeVM:       env("KEERA_AGENTS_RUNTIME_VM", ""),
			Namespace:       env("KEERA_AGENTS_NAMESPACE", ""),
			StorageClass:    env("KEERA_AGENTS_STORAGE_CLASS", ""),
			ServiceAccount:  env("KEERA_AGENTS_SERVICE_ACCOUNT", ""),
			PodmanBinary:    env("KEERA_AGENTS_PODMAN_BINARY", "podman"),
			PodmanNetwork:   env("KEERA_AGENTS_PODMAN_NETWORK", ""),
			MinIsolation:    env("KEERA_AGENTS_MIN_ISOLATION", "standard"),
			MaxCPU:          env("KEERA_AGENTS_MAX_CPU", "8"),
			MaxMemory:       env("KEERA_AGENTS_MAX_MEMORY", "32Gi"),
			MaxDisk:         env("KEERA_AGENTS_MAX_DISK", "100Gi"),
		},
	}
	if v := env("KEERA_AGENTS_IMAGE_PULL_SECRETS", ""); v != "" {
		for s := range strings.SplitSeq(v, ",") {
			if s = strings.TrimSpace(s); s != "" {
				c.Runtime.ImagePullSecrets = append(c.Runtime.ImagePullSecrets, s)
			}
		}
	}

	var err error
	if c.GatewayURL, err = originEnv("KEERA_AGENTS_GATEWAY_URL", ""); err != nil {
		return c, err
	}
	if c.GatewayURL == "" {
		return c, errors.New("KEERA_AGENTS_GATEWAY_URL is required: the address a browser " +
			"reaches Keera Gateway on, such as https://keera.example.ch")
	}
	if c.GatewayInternalURL, err = originEnv("KEERA_AGENTS_GATEWAY_INTERNAL_URL", c.GatewayURL); err != nil {
		return c, err
	}

	// Loopback by default: the gateway allows it without setup.
	def := ""
	if _, port, err := net.SplitHostPort(c.Addr); err == nil && port != "" {
		def = "http://127.0.0.1:" + port
	}
	if c.PublicURL, err = originEnv("KEERA_AGENTS_PUBLIC_URL", def); err != nil {
		return c, err
	}
	if c.PublicURL == "" {
		return c, errors.New("KEERA_AGENTS_PUBLIC_URL is required: the address a browser " +
			"reaches keera-agents on, such as https://agents.example.ch")
	}
	if c.Runtime.URL, err = originEnv("KEERA_AGENTS_RUNTIME_URL", c.PublicURL); err != nil {
		return c, err
	}
	if c.Runtime.GatewayURL, err = originEnv("KEERA_AGENTS_RUNTIME_GATEWAY_URL", c.GatewayInternalURL); err != nil {
		return c, err
	}

	if len(c.ServiceKey) < 32 {
		return c, errors.New("KEERA_AGENTS_SERVICE_KEY is required and must be at least 32 " +
			"characters; set the same value as KEERA_AGENTS_SERVICE_KEY on Keera Gateway")
	}
	if c.DatabaseURL == "" {
		return c, errors.New("KEERA_AGENTS_DATABASE_URL is required, such as " +
			"postgres://keera:secret@db:5432/keera_agents")
	}
	if len(c.SecretKey) < 16 {
		return c, errors.New("KEERA_AGENTS_SECRET_KEY is required and must be at least 16 " +
			"characters; generate one with: openssl rand -hex 32")
	}

	if c.MetricsToken != "" {
		switch {
		case len(c.MetricsToken) < 16:
			return c, errors.New("KEERA_AGENTS_METRICS_TOKEN is too short to be a credential; " +
				"generate one with: openssl rand -hex 32")
		case c.MetricsToken == c.ServiceKey || c.MetricsToken == c.SecretKey:
			return c, errors.New("KEERA_AGENTS_METRICS_TOKEN must differ from KEERA_AGENTS_SERVICE_KEY " +
				"and KEERA_AGENTS_SECRET_KEY, so a scrape configuration holds nothing else")
		}
	}

	if c.SecureCookies, err = boolEnv("KEERA_AGENTS_SECURE_COOKIES", strings.HasPrefix(c.PublicURL, "https://")); err != nil {
		return c, err
	}
	if c.Runtime.UserNamespaces, err = boolEnv("KEERA_AGENTS_USER_NAMESPACES", false); err != nil {
		return c, err
	}

	if c.Runtime.IdleSuspend, err = durationEnv("KEERA_AGENTS_IDLE_SUSPEND", 15*time.Minute, time.Minute); err != nil {
		return c, err
	}
	if c.Runtime.WorkspaceIdleSuspend, err = durationEnv("KEERA_AGENTS_WORKSPACE_IDLE_SUSPEND", 2*time.Hour, time.Minute); err != nil {
		return c, err
	}
	if c.TaskTimeout, err = durationEnv("KEERA_AGENTS_TASK_TIMEOUT", 30*time.Minute, time.Minute); err != nil {
		return c, err
	}
	if c.TaskTimeout == 0 {
		return c, errors.New("KEERA_AGENTS_TASK_TIMEOUT cannot be 0: a task must end some time")
	}
	if c.WatchInterval, err = durationEnv("KEERA_AGENTS_WATCH_INTERVAL", 5*time.Minute, 30*time.Second); err != nil {
		return c, err
	}
	if c.WatchInterval == 0 {
		return c, errors.New("KEERA_AGENTS_WATCH_INTERVAL cannot be 0")
	}

	if c.Egress.Deny, err = egress.ParsePrefixes(env("KEERA_AGENTS_UPSTREAM_DENY", egress.DefaultDeny)); err != nil {
		return c, fmt.Errorf("KEERA_AGENTS_UPSTREAM_DENY: %w", err)
	}
	if c.Egress.LimitPrivate, c.Egress.PrivateHosts, err = egress.ParsePrivateHosts(
		env("KEERA_AGENTS_UPSTREAM_PRIVATE", "none")); err != nil {
		return c, fmt.Errorf("KEERA_AGENTS_UPSTREAM_PRIVATE: %w", err)
	}

	for _, p := range []struct {
		key string
		to  *int64
	}{
		{"KEERA_AGENTS_PRICE_CPU_HOUR", &c.PriceCPUHour},
		{"KEERA_AGENTS_PRICE_MEMORY_GIB_HOUR", &c.PriceMemoryGiBHour},
		{"KEERA_AGENTS_PRICE_DISK_GIB_MONTH", &c.PriceDiskGiBMonth},
	} {
		if *p.to, err = price(p.key, env(p.key, "")); err != nil {
			return c, err
		}
	}

	if !slices.Contains([]string{"", "podman", "kubernetes"}, c.Runtime.Driver) {
		return c, fmt.Errorf("KEERA_AGENTS_DRIVER must be podman, kubernetes or empty, not %q", c.Runtime.Driver)
	}
	// In keera-agents' own namespace, an agent's shell could reach the
	// database, and anyone who reads pods there reads agents' tokens.
	if c.Runtime.Driver == "kubernetes" && c.Runtime.Namespace == "" {
		return c, errors.New("KEERA_AGENTS_NAMESPACE must name where agents run: " +
			"a namespace of their own, with the NetworkPolicy in docs/agents.md#hardening")
	}
	if !slices.Contains([]string{"debug", "info", "warn", "error"}, c.LogLevel) {
		return c, fmt.Errorf("KEERA_AGENTS_LOG_LEVEL must be debug, info, warn or error, not %q", c.LogLevel)
	}
	if c.LogFormat != "text" && c.LogFormat != "json" {
		return c, fmt.Errorf("KEERA_AGENTS_LOG_FORMAT must be text or json, not %q", c.LogFormat)
	}
	return c, nil
}

// maxPrice bounds a price, in CHF, so a slip such as a price in centimes
// stops the start instead of billing a hundred times too much.
const maxPrice = 1000

// price reads a price in CHF, such as 0.05, as micro-CHF. Empty is 0, free.
func price(key, raw string) (int64, error) {
	if raw == "" {
		return 0, nil
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil || math.IsNaN(v) || v < 0 || v > maxPrice {
		return 0, fmt.Errorf("%s must be a price in CHF from 0 to %d, such as 0.05, not %q", key, maxPrice, raw)
	}
	return int64(math.Round(v * 1e6)), nil
}

// origin returns raw as a bare http(s) origin. Paths are refused: both
// keera-agents and the gateway run at the root of their host.
func origin(name, raw string) (string, error) {
	if raw == "" {
		return "", nil
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" ||
		u.User != nil || u.RawQuery != "" || u.Fragment != "" ||
		strings.Trim(u.Path, "/") != "" {
		return "", fmt.Errorf("%s must be an http or https address without a path, "+
			"such as https://host.example.ch, not %q", name, raw)
	}
	return u.Scheme + "://" + u.Host, nil
}
