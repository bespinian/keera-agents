package billing

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bespinian/keera-agents/internal/gateway"
	"github.com/bespinian/keera-agents/internal/runtime"
	"github.com/bespinian/keera-agents/internal/store"
)

func TestLines(t *testing.T) {
	p := Prices{CPUHour: 50_000, MemoryGiBHour: 6_000, DiskGiBMonth: 120_000}
	hour := time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)
	// Two CPUs and 4 GiB for the whole hour, and 10 GiB of disk.
	h := store.OrgHour{
		OrgID: "org_a", Hour: hour,
		CPUMilliSeconds:  2000 * 3600,
		MemoryMiBSeconds: 4096 * 3600,
		DiskMiBSeconds:   10240 * 3600,
	}
	lines := p.lines(h)
	if len(lines) != 3 {
		t.Fatalf("got %d lines, want 3: %+v", len(lines), lines)
	}
	want := []struct {
		id, item, unit   string
		quantity, micros int64
	}{
		{"agents/org_a/2026-11-17T10:00:00Z/cpu", "CPU", "CPU-hours", 2_000_000, 100_000},
		{"agents/org_a/2026-11-17T10:00:00Z/memory", "Memory", "GiB-hours", 4_000_000, 24_000},
		// 10 GiB for one of 730 hours of a month, at 0.12 a GiB-month.
		{"agents/org_a/2026-11-17T10:00:00Z/disk", "Disk", "GiB-months", 13_699, 1_644},
	}
	for i, w := range want {
		l := lines[i]
		if l.ID != w.id || l.Item != w.item || l.Unit != w.unit || l.OrgID != "org_a" || !l.TS.Equal(hour) ||
			l.QuantityMicros != w.quantity || l.Micros != w.micros {
			t.Errorf("line %d = %+v, want %+v", i, l, w)
		}
	}
	if got := p.Cost(h.MachineUse); got != 100_000+24_000+1_644 {
		t.Errorf("Cost = %d, want the lines' sum", got)
	}
}

func TestLinesSkipFreeAndUnused(t *testing.T) {
	p := Prices{CPUHour: 50_000}
	h := store.OrgHour{OrgID: "org_a", DiskMiBSeconds: 1 << 30}
	if lines := p.lines(h); len(lines) != 0 {
		t.Errorf("disk is free and no CPU ran, yet: %+v", lines)
	}
}

func TestScale(t *testing.T) {
	if got := scale(5, 1, 10); got != 1 {
		t.Errorf("a half rounds up: %d", got)
	}
	if got := scale(4, 1, 10); got != 0 {
		t.Errorf("less than a half rounds down: %d", got)
	}
	// A month of 100 TiB of disk at CHF 1000 a GiB-month would overflow an
	// int64 before dividing.
	q := int64(100<<20) * 3600 * 730
	if got := scale(q, 1_000_000_000, 1024*3600*730); got != 100*1024*1_000_000_000 {
		t.Errorf("large: %d", got)
	}
	if q <= math.MaxInt64/1_000_000_000 {
		t.Fatal("the case no longer overflows; make it larger")
	}
}

func TestMeasure(t *testing.T) {
	a := store.Agent{Class: runtime.Class{CPU: "2", Memory: "1Gi", Disk: "10Gi"}, State: store.AgentRunning, HasDisk: true}
	if got := measure(a, 60); got != (store.MachineUse{
		CPUMilliSeconds: 120_000, MemoryMiBSeconds: 61_440,
		DiskMiBSeconds: 614_400,
	}) {
		t.Errorf("running: %+v", got)
	}
	for _, state := range []string{store.AgentStarting, store.AgentSuspended, store.AgentStopped, store.AgentFailed} {
		a.State = state
		if got := measure(a, 60); got != (store.MachineUse{DiskMiBSeconds: 614_400}) {
			t.Errorf("%s: %+v, want disk only", state, got)
		}
	}
	// A machine never made, as one refused for credit, has no disk to bill.
	a.State, a.HasDisk = store.AgentStopped, false
	if got := measure(a, 60); !got.Zero() {
		t.Errorf("no machine: %+v, want nothing", got)
	}
}

func TestHour(t *testing.T) {
	// A CHF a CPU-hour, 10 centimes a GiB-hour of memory, and CHF 7.30 a
	// GiB-month of disk, which is a centime an hour.
	p := Prices{CPUHour: 1_000_000, MemoryGiBHour: 100_000, DiskGiBMonth: 7_300_000}
	a := store.Agent{Class: runtime.Class{CPU: "500m", Memory: "2Gi", Disk: "1Gi"}, State: store.AgentStopped}
	// A stopped machine is priced as it runs: 0.50 + 0.20 + 0.01.
	if got := p.Hour(a); got != 710_000 {
		t.Errorf("an hour = %d, want 710000", got)
	}
}

// fakeGateway answers GET /control/v1/service/billing with credit, and
// counts the calls.
func fakeGateway(t *testing.T, credit gateway.Credit) (*gateway.Client, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/control/v1/service/billing" || r.URL.Query().Get("org_id") != "org_a" {
			t.Errorf("unexpected call %s", r.URL)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(credit)
	}))
	t.Cleanup(srv.Close)
	return gateway.New(srv.URL, "service-key"), &calls
}

func TestMayRun(t *testing.T) {
	ctx := context.Background()
	refused := gateway.Credit{Payments: true, Refusal: &gateway.Refusal{
		Code: "credit_exhausted", Message: "your organisation has used up its credit",
	}}
	gw, calls := fakeGateway(t, refused)

	off := New(nil, gw, Prices{}, nil)
	if err := off.MayRun(ctx, "org_a"); err != nil || calls.Load() != 0 {
		t.Fatalf("without prices, nothing is asked or refused: %v, %d calls", err, calls.Load())
	}

	b := New(nil, gw, Prices{CPUHour: 1}, nil)
	err := b.MayRun(ctx, "org_a")
	if !IsRefused(err) || err.Error() != refused.Refusal.Message {
		t.Fatalf("refused: %v", err)
	}
	_ = b.MayRun(ctx, "org_a")
	if calls.Load() != 1 {
		t.Errorf("a fresh answer is used again, yet the gateway was asked %d times", calls.Load())
	}
}

func TestMayRunWhenGatewayIsAway(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	srv.Close()
	b := New(nil, gateway.New(srv.URL, "k"), Prices{CPUHour: 1}, nil)
	if err := b.MayRun(context.Background(), "org_a"); err != nil {
		t.Errorf("without any answer, a machine starts: %v", err)
	}
	// The last answer counts while the gateway is away.
	b.credit["org_a"] = answer{at: time.Now().Add(-time.Hour), credit: gateway.Credit{
		Refusal: &gateway.Refusal{Code: "charge_due", Message: "due"},
	}}
	if err := b.MayRun(context.Background(), "org_a"); !IsRefused(err) {
		t.Errorf("the last answer refused: %v", err)
	}
}
