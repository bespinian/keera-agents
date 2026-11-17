// Package runtime runs agents' containers: one persistent container per
// agent, with a volume for its workspace. It is suspended when the agent has
// nothing to do and resumed when a task arrives.
//
// Everything is behind Driver. The Kubernetes driver makes a StatefulSet per
// agent, so it needs nothing installed on the cluster. The podman driver runs
// local containers, for development.
//
// The drivers come from Keera Gateway's sandboxes, which keera-agents
// replaces.
package runtime

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
)

var (
	// ErrNotFound is Suspend, Resume, Logs or Address finding no container.
	ErrNotFound = errors.New("runtime: not found")
	// ErrTier is a class asking for an isolation tier this deployment has
	// not mapped. It is refused, never run with less.
	ErrTier = errors.New("runtime: isolation tier not available")
)

// Isolation is how strongly an agent is kept from the host.
type Isolation string

const (
	// Standard shares the host kernel, as any pod or container does.
	Standard Isolation = "standard"
	// Isolated is gVisor: syscalls served in userspace.
	Isolated Isolation = "isolated"
	// VM is Kata, or krun on podman: a kernel of its own.
	VM Isolation = "vm"
)

func (i Isolation) Valid() bool { return i == Standard || i == Isolated || i == VM }

// AtLeast says whether i isolates as strongly as min does.
func (i Isolation) AtLeast(min Isolation) bool {
	rank := map[Isolation]int{Standard: 0, Isolated: 1, VM: 2}
	return rank[i] >= rank[min]
}

// Class is the machine an agent runs on.
type Class struct {
	Isolation Isolation `json:"isolation"`
	// CPU and Memory are Kubernetes quantities, such as "2" and "4Gi".
	CPU    string `json:"cpu"`
	Memory string `json:"memory"`
	// Disk sizes the workspace volume on Kubernetes, such as "10Gi". Podman
	// volumes are unsized.
	Disk string `json:"disk"`
}

// DefaultClass is what an agent gets when nobody chose.
var DefaultClass = Class{Isolation: Isolated, CPU: "1", Memory: "2Gi", Disk: "5Gi"}

// WithDefaults fills each field left empty from DefaultClass. It is the one
// place defaults apply, so the panel and the drivers agree.
func (c Class) WithDefaults() Class {
	c.Isolation = cmp.Or(c.Isolation, DefaultClass.Isolation)
	c.CPU = cmp.Or(strings.TrimSpace(c.CPU), DefaultClass.CPU)
	c.Memory = cmp.Or(strings.TrimSpace(c.Memory), DefaultClass.Memory)
	c.Disk = cmp.Or(strings.TrimSpace(c.Disk), DefaultClass.Disk)
	return c
}

// Validate checks a class with its defaults filled in. Its errors are worded
// for the person who chose the class.
func (c Class) Validate() error {
	if !c.Isolation.Valid() {
		return fmt.Errorf("isolation must be standard, isolated or vm, not %q", c.Isolation)
	}
	// Zero would mean no limit at all to both drivers. So would a value the
	// podman driver rounds down to zero, such as a memory of 500m bytes.
	for _, q := range []struct {
		name, v, min string
		atLeast      float64
	}{
		{"CPU", c.CPU, "10m", 0.01},
		{"memory", c.Memory, "64Mi", 64 << 20},
		{"disk", c.Disk, "64Mi", 64 << 20},
	} {
		n, err := parseQuantity(q.v)
		if err != nil || n <= 0 {
			return fmt.Errorf("%s must be a size such as 2, 500m or 4Gi, not %q", q.name, q.v)
		}
		if n < q.atLeast {
			return fmt.Errorf("%s must be at least %s, not %q", q.name, q.min, q.v)
		}
	}
	return nil
}

// Size is a class in whole units, for metering: CPU in millicores, memory and
// disk in MiB. A field that does not parse is 0, which Validate refuses
// before any machine starts.
func (c Class) Size() (cpuMillis, memoryMiB, diskMiB int64) {
	c = c.WithDefaults()
	cpu, _ := parseQuantity(c.CPU)
	mem, _ := parseQuantity(c.Memory)
	disk, _ := parseQuantity(c.Disk)
	return int64(math.Round(cpu * 1000)), int64(math.Round(mem / (1 << 20))), int64(math.Round(disk / (1 << 20)))
}

// Limits bound the classes agents may ask for, so one organisation's agent
// cannot take the machines others share. The operator sets them.
type Limits struct {
	MinIsolation Isolation
	// Quantities, such as "8" and "32Gi".
	MaxCPU, MaxMemory, MaxDisk string
}

// Check refuses a class outside the limits. A zero Limits allows any valid
// class. Its errors are worded for the person who chose the class.
func (l Limits) Check(c Class) error {
	if err := c.Validate(); err != nil {
		return err
	}
	if l.MinIsolation != "" && !c.Isolation.AtLeast(l.MinIsolation) {
		return fmt.Errorf("this deployment runs agents with %s isolation or stronger", l.MinIsolation)
	}
	for _, q := range []struct{ name, v, max string }{
		{"CPU", c.CPU, l.MaxCPU}, {"memory", c.Memory, l.MaxMemory}, {"disk", c.Disk, l.MaxDisk},
	} {
		if q.max == "" {
			continue
		}
		v, _ := parseQuantity(q.v)
		if max, err := parseQuantity(q.max); err == nil && v > max {
			return fmt.Errorf("%s can be at most %s on this deployment", q.name, q.max)
		}
	}
	return nil
}

// Spec is everything a driver needs to make one agent's container. Its class
// has its defaults filled in, so drivers make no decisions and behave alike.
type Spec struct {
	// AgentID names the container and volume, so a restarted control plane
	// finds the containers it left running.
	AgentID string
	// Workspace is a machine an engineer works in. Unlike an agent's, its
	// container takes connections from keera-agents on protocol.FrontPort.
	Workspace bool
	// Name is what people called the agent. It shows in kubectl and podman ps.
	Name  string
	Image string
	Class Class
	// Owner, Org and Project are stamped on as labels or annotations, so
	// operators see whose it is. Owner is an email address.
	Owner   string
	Org     string
	Project string
	// Env holds the runtime token, never the gateway key: the runtime fetches
	// that at start.
	Env map[string]string
}

type State string

const (
	Pending   State = "pending"
	Running   State = "running"
	Suspended State = "suspended"
	Failed    State = "failed"
	// Gone means the driver has no container for the agent.
	Gone State = "gone"
)

// Status is what a driver knows about one agent's container right now.
type Status struct {
	State State
	// Detail says why a state is not Running, in the driver's own words.
	Detail string
	// Token is the runtime token a running container was given, so the fleet
	// can tell one that is no longer valid. Empty if unknown. Never log it.
	Token string
}

// Driver runs agent containers on one platform. Methods are idempotent where
// they can be, which keeps rows and reality in step after a dropped
// connection.
type Driver interface {
	// Name is "kubernetes" or "podman".
	Name() string
	// Tiers lists standard and each tier with a runtime mapped.
	Tiers() []Isolation
	// Create makes and starts the container and its volume, and returns
	// without waiting for it to be ready. Creating one that exists replaces
	// the container and keeps the volume, so a changed image or class takes
	// effect.
	Create(ctx context.Context, spec Spec) (Status, error)
	// Status reports a missing container as Gone, not as an error.
	Status(ctx context.Context, agentID string) (Status, error)
	// Suspend releases the compute and keeps the volume. Resume undoes it.
	Suspend(ctx context.Context, agentID string) error
	Resume(ctx context.Context, agentID string) error
	// Terminate removes the container and its volume. One already gone is
	// not an error.
	Terminate(ctx context.Context, agentID string) error
	// Logs streams what the runtime printed, and with follow what it prints
	// next, until ctx ends. ErrNotFound means there is no container.
	Logs(ctx context.Context, agentID string, follow bool) (io.ReadCloser, error)
	// Address is host:port of a running workspace's front, as keera-agents
	// reaches it. ErrNotFound means it has no running container.
	Address(ctx context.Context, agentID string) (string, error)
}
