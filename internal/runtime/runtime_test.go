package runtime

import (
	"strings"
	"testing"

	"github.com/bespinian/keera-agents/internal/config"
)

func TestClassWithDefaults(t *testing.T) {
	if got := (Class{}).WithDefaults(); got != DefaultClass {
		t.Errorf("an empty class = %+v, want the default", got)
	}
	got := Class{Isolation: Standard, CPU: " 2 "}.WithDefaults()
	want := Class{Isolation: Standard, CPU: "2", Memory: DefaultClass.Memory, Disk: DefaultClass.Disk}
	if got != want {
		t.Errorf("a partial class = %+v, want %+v", got, want)
	}
}

func TestClassValidate(t *testing.T) {
	if err := DefaultClass.Validate(); err != nil {
		t.Errorf("the default class: %v", err)
	}
	for _, c := range []Class{
		{},
		{Isolation: "strong", CPU: "1", Memory: "1Gi", Disk: "1Gi"},
		{Isolation: Standard, CPU: "lots", Memory: "1Gi", Disk: "1Gi"},
		{Isolation: Standard, CPU: "1", Memory: "1Gi", Disk: ""},
		// Zero is no limit to both drivers.
		{Isolation: Standard, CPU: "0", Memory: "1Gi", Disk: "1Gi"},
		{Isolation: Standard, CPU: "1", Memory: "0", Disk: "1Gi"},
		// Too small to mean anything; podman would round these to zero.
		{Isolation: Standard, CPU: "1n", Memory: "1Gi", Disk: "1Gi"},
		{Isolation: Standard, CPU: "1", Memory: "500m", Disk: "1Gi"},
		{Isolation: Standard, CPU: "1", Memory: "1Gi", Disk: "1k"},
	} {
		if err := c.Validate(); err == nil {
			t.Errorf("%+v was accepted", c)
		}
	}
}

func TestLimits(t *testing.T) {
	l, err := LimitsFrom(config.RuntimeConfig{MinIsolation: "isolated", MaxCPU: "4", MaxMemory: "8Gi", MaxDisk: "50Gi"})
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Check(DefaultClass); err != nil {
		t.Errorf("the default class: %v", err)
	}
	if err := l.Check(Class{Isolation: VM, CPU: "4", Memory: "8Gi", Disk: "50Gi"}); err != nil {
		t.Errorf("at the limits: %v", err)
	}
	for want, c := range map[string]Class{
		"isolated isolation or stronger": {Isolation: Standard, CPU: "1", Memory: "1Gi", Disk: "1Gi"},
		"CPU can be at most 4":           {Isolation: VM, CPU: "4500m", Memory: "1Gi", Disk: "1Gi"},
		"memory can be at most 8Gi":      {Isolation: VM, CPU: "1", Memory: "9Gi", Disk: "1Gi"},
		"disk can be at most 50Gi":       {Isolation: VM, CPU: "1", Memory: "1Gi", Disk: "1Ti"},
		"memory must be a size":          {Isolation: VM, CPU: "1", Memory: "0", Disk: "1Gi"},
		"memory must be at least 64Mi":   {Isolation: VM, CPU: "1", Memory: "500m", Disk: "1Gi"},
	} {
		if err := l.Check(c); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%+v: err = %v, want %q", c, err, want)
		}
	}
	for _, bad := range []config.RuntimeConfig{
		{MinIsolation: "strong", MaxCPU: "4", MaxMemory: "8Gi", MaxDisk: "50Gi"},
		{MinIsolation: "standard", MaxCPU: "0", MaxMemory: "8Gi", MaxDisk: "50Gi"},
		{MinIsolation: "standard", MaxCPU: "4", MaxMemory: "lots", MaxDisk: "50Gi"},
	} {
		if _, err := LimitsFrom(bad); err == nil {
			t.Errorf("%+v was accepted", bad)
		}
	}
}
