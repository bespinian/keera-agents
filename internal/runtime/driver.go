package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"strings"

	"github.com/bespinian/keera-agents/internal/config"
)

// New makes the driver cfg.Driver names. It returns a nil Driver and no error
// when cfg.Driver is empty: agents can then be set up but not started.
func New(ctx context.Context, cfg config.RuntimeConfig, log *slog.Logger) (Driver, error) {
	runtimes := map[Isolation]string{
		Standard: cfg.RuntimeStandard,
		Isolated: cfg.RuntimeIsolated,
		VM:       cfg.RuntimeVM,
	}
	switch cfg.Driver {
	case "":
		return nil, nil
	case "podman":
		return NewPodman(ctx, PodmanOptions{
			Binary:   cfg.PodmanBinary,
			Network:  cfg.PodmanNetwork,
			Runtimes: runtimes,
		})
	case "kubernetes":
		return NewKubernetes(ctx, KubernetesOptions{
			Namespace:        cfg.Namespace,
			Runtimes:         runtimes,
			StorageClass:     cfg.StorageClass,
			ServiceAccount:   cfg.ServiceAccount,
			ImagePullSecrets: cfg.ImagePullSecrets,
			UserNamespaces:   cfg.UserNamespaces,
			Log:              log,
		})
	default:
		return nil, fmt.Errorf("unknown runtime driver %q; use podman or kubernetes", cfg.Driver)
	}
}

// LimitsFrom reads and checks the operator's limits.
func LimitsFrom(cfg config.RuntimeConfig) (Limits, error) {
	l := Limits{
		MinIsolation: Isolation(cfg.MinIsolation),
		MaxCPU:       cfg.MaxCPU, MaxMemory: cfg.MaxMemory, MaxDisk: cfg.MaxDisk,
	}
	if !l.MinIsolation.Valid() {
		return l, fmt.Errorf("KEERA_AGENTS_MIN_ISOLATION must be standard, isolated or vm, not %q", l.MinIsolation)
	}
	for _, q := range []struct{ name, v string }{
		{"KEERA_AGENTS_MAX_CPU", l.MaxCPU}, {"KEERA_AGENTS_MAX_MEMORY", l.MaxMemory}, {"KEERA_AGENTS_MAX_DISK", l.MaxDisk},
	} {
		if n, err := parseQuantity(q.v); err != nil || n <= 0 {
			return l, fmt.Errorf("%s must be a size such as 8 or 32Gi, not %q", q.name, q.v)
		}
	}
	return l, nil
}

// tiersFor derives the tiers from the runtimes, so a driver cannot claim a
// tier it has no runtime for. Weakest first.
func tiersFor(runtimes map[Isolation]string) []Isolation {
	tiers := []Isolation{Standard}
	for _, tier := range []Isolation{Isolated, VM} {
		if runtimes[tier] != "" {
			tiers = append(tiers, tier)
		}
	}
	return tiers
}

// runtimeFor returns the runtime for a class's tier. Empty means the
// platform's default, which only the standard tier may use: an unmapped
// stronger tier is refused, never run with less. checkSpec has checked the
// tier.
func runtimeFor(c Class, runtimes map[Isolation]string, driver string) (string, error) {
	tier := c.Isolation
	if tier == Standard {
		return runtimes[Standard], nil
	}
	if name := runtimes[tier]; name != "" {
		return name, nil
	}
	return "", fmt.Errorf("%w: the class asks for %s isolation and the %s driver has no runtime "+
		"for it; set KEERA_AGENTS_RUNTIME_%s (a RuntimeClass on Kubernetes, runsc or krun on podman), "+
		"or choose a tier it has", ErrTier, tier, driver, strings.ToUpper(string(tier)))
}

// The labels on everything a driver makes, so operators see whose it is.
const (
	labelManagedBy = "app.kubernetes.io/managed-by"
	labelName      = "app.kubernetes.io/name"
	labelAgentID   = "keera.dev/agent-id"
	labelOrg       = "keera.dev/org"
	labelProject   = "keera.dev/project"
	// Owner and agent name are annotations: a label value cannot hold an @ or
	// a space.
	annotationOwner     = "keera.dev/owner"
	annotationAgentName = "keera.dev/agent-name"
	// annotationSpecHash tells an unchanged spec from a changed one, so Create
	// restarts an agent only when something it runs with changed.
	annotationSpecHash = "keera.dev/spec-hash"
)

func labelsFor(spec Spec) map[string]string {
	labels := map[string]string{
		labelManagedBy: "keera-agents",
		labelName:      "keera-agent",
		labelAgentID:   labelValue(spec.AgentID),
	}
	if v := labelValue(spec.Org); v != "" {
		labels[labelOrg] = v
	}
	if v := labelValue(spec.Project); v != "" {
		labels[labelProject] = v
	}
	return labels
}

func annotationsFor(spec Spec) map[string]string {
	annotations := map[string]string{}
	if spec.Owner != "" {
		annotations[annotationOwner] = spec.Owner
	}
	if spec.Name != "" {
		annotations[annotationAgentName] = spec.Name
	}
	return annotations
}

// labelValue keeps what a Kubernetes label value allows: 63 characters of
// letters, digits, '-', '_' and '.', starting and ending with a letter or
// digit. Identifiers pass unchanged.
func labelValue(s string) string {
	b := []byte(s)
	for i, c := range b {
		if !isAlnum(c) && c != '-' && c != '_' && c != '.' {
			b[i] = '_'
		}
	}
	if len(b) > 63 {
		b = b[:63]
	}
	return strings.TrimFunc(string(b), func(r rune) bool { return r > 127 || !isAlnum(byte(r)) })
}

func isAlnum(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

// maxName leaves room for what a StatefulSet adds: its pod carries a
// controller-revision-hash label of the name, a dash and up to ten characters,
// and a label value holds at most 63.
const maxName = 52

// objectName names an agent's container, volume and StatefulSet. It depends only
// on the agent's id, so a restarted control plane finds what it left running.
// It is a DNS label: lower case, digits and '-'.
func objectName(agentID string) string {
	const prefix = "keera-agent-"
	b := []byte(strings.ToLower(agentID))
	for i, c := range b {
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') {
			b[i] = '-'
		}
	}
	id := strings.Trim(string(b), "-")
	if len(prefix)+len(id) > maxName {
		// Two long ids that share a start must still differ.
		sum := sha256.Sum256([]byte(agentID))
		id = strings.TrimRight(id[:maxName-len(prefix)-9], "-") + "-" + hex.EncodeToString(sum[:4])
	}
	return prefix + id
}

func checkSpec(spec Spec) error {
	if spec.AgentID == "" {
		return fmt.Errorf("runtime: an agent needs an id")
	}
	if spec.Image == "" {
		return fmt.Errorf("runtime: agent %s has no image", spec.AgentID)
	}
	if err := spec.Class.Validate(); err != nil {
		return fmt.Errorf("runtime: agent %s: %w", spec.AgentID, err)
	}
	return nil
}

// parseQuantity reads a Kubernetes quantity such as "500m", "2", "4Gi" or
// "1.5G". Podman needs plain numbers, and Kubernetes would refuse a bad one
// only after the object is half made.
func parseQuantity(s string) (float64, error) {
	s = strings.TrimSpace(s)
	mult := 1.0
	for _, suf := range []struct {
		s string
		m float64
	}{
		{"Ki", 1 << 10},
		{"Mi", 1 << 20},
		{"Gi", 1 << 30},
		{"Ti", 1 << 40},
		{"Pi", 1 << 50},
		{"Ei", 1 << 60},
		{"n", 1e-9},
		{"u", 1e-6},
		{"m", 1e-3},
		{"k", 1e3},
		{"M", 1e6},
		{"G", 1e9},
		{"T", 1e12},
		{"P", 1e15},
		{"E", 1e18},
	} {
		if before, ok := strings.CutSuffix(s, suf.s); ok {
			s, mult = before, suf.m
			break
		}
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || v < 0 || math.IsInf(v, 0) || math.IsNaN(v) || s == "" {
		return 0, fmt.Errorf("not a quantity such as 500m, 2 or 4Gi")
	}
	return v * mult, nil
}

func hashOf(parts ...[]byte) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write(p)
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}
