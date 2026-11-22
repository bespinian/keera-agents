// Package billing meters agents' and workspaces' machines and bills them to
// Keera Gateway, which holds each organisation's credit and takes its
// payments. keera-agents takes no money itself. See docs/billing.md.
//
// Each minute the leader meters the machines: CPU and memory while one runs,
// disk while it exists, into one row per machine and hour. Once an hour is
// over, it goes to the gateway as one line per organisation and item, with
// an id that makes sending it again harmless. Before a machine starts, the
// gateway is asked whether its organisation may run one.
package billing

import (
	"context"
	"errors"
	"log/slog"
	"math/big"
	"strings"
	"sync"
	"time"

	"github.com/bespinian/keera-agents/internal/gateway"
	"github.com/bespinian/keera-agents/internal/store"
)

const (
	secondsPerHour = 3600
	// A month of disk is 730 hours, a year's twelfth, as cloud providers
	// count it. So a month costs the same whatever its length.
	hoursPerMonth = 730

	// How many of store.MachineUse's units make one unit on the bill.
	milliSecondsPerCPUHour = 1000 * secondsPerHour
	mibSecondsPerGiBHour   = 1024 * secondsPerHour
	mibSecondsPerGiBMonth  = mibSecondsPerGiBHour * hoursPerMonth

	// maxGap is the most time one round meters. Longer, nobody saw what
	// ran: the control plane was down.
	maxGap = 15 * time.Minute
	// maxAge is how old an hour may be and still be sent. The gateway takes
	// lines up to 40 days old.
	maxAge = 39 * 24 * time.Hour
	// Retention is how long machine time is kept here. The gateway keeps
	// the bill.
	Retention = 100 * 24 * time.Hour
	// hoursPerCall keeps each call within gateway.MaxBillLines, at three
	// lines an hour.
	hoursPerCall = gateway.MaxBillLines / 3

	// creditFresh is how long the gateway's answer on credit is used. A
	// payment therefore lets machines start within a minute.
	creditFresh = time.Minute
	// warnEvery spaces out warnings that would otherwise come each round.
	warnEvery = time.Hour
)

// Prices are what machine time costs, in micro-CHF. Zero is free. With
// every price zero, billing is off.
type Prices struct {
	CPUHour       int64 `json:"cpu_hour_micros"`
	MemoryGiBHour int64 `json:"memory_gib_hour_micros"`
	// DiskGiBMonth is per GiB and month of 730 hours.
	DiskGiBMonth int64 `json:"disk_gib_month_micros"`
}

func (p Prices) On() bool { return p.CPUHour > 0 || p.MemoryGiBHour > 0 || p.DiskGiBMonth > 0 }

// item is one thing a line bills: its name and unit on the bill, its price
// per unit, and how many of store.MachineUse's units make one.
type item struct {
	name, unit string
	price      int64
	per        int64
	of         func(store.MachineUse) int64
}

func (p Prices) items() []item {
	return []item{
		{
			"CPU", "CPU-hours", p.CPUHour, milliSecondsPerCPUHour,
			func(u store.MachineUse) int64 { return u.CPUMilliSeconds },
		},
		{
			"Memory", "GiB-hours", p.MemoryGiBHour, mibSecondsPerGiBHour,
			func(u store.MachineUse) int64 { return u.MemoryMiBSeconds },
		},
		{
			"Disk", "GiB-months", p.DiskGiBMonth, mibSecondsPerGiBMonth,
			func(u store.MachineUse) int64 { return u.DiskMiBSeconds },
		},
	}
}

// Quantities is u in the units the bill counts, for the panel.
func Quantities(u store.MachineUse) (cpuHours, memoryGiBHours, diskGiBMonths float64) {
	return float64(u.CPUMilliSeconds) / milliSecondsPerCPUHour,
		float64(u.MemoryMiBSeconds) / mibSecondsPerGiBHour,
		float64(u.DiskMiBSeconds) / mibSecondsPerGiBMonth
}

// lines are the gateway's bill lines for one organisation's hour: one per
// item with a price, and only for what was used.
func (p Prices) lines(h store.OrgHour) []gateway.BillLine {
	var out []gateway.BillLine
	for _, it := range p.items() {
		q := it.of(h.MachineUse)
		if it.price == 0 || q == 0 {
			continue
		}
		out = append(out, gateway.BillLine{
			ID:             "agents/" + h.OrgID + "/" + h.Hour.UTC().Format(time.RFC3339) + "/" + strings.ToLower(it.name),
			OrgID:          h.OrgID,
			TS:             h.Hour,
			Item:           it.name,
			Unit:           it.unit,
			QuantityMicros: scale(q, 1_000_000, it.per),
			Micros:         scale(q, it.price, it.per),
		})
	}
	return out
}

// Cost is what u costs at these prices, as the lines would bill it.
func (p Prices) Cost(u store.MachineUse) int64 {
	var sum int64
	for _, it := range p.items() {
		sum += scale(it.of(u), it.price, it.per)
	}
	return sum
}

// Hour is what an hour of a's machine running costs at its size now: CPU,
// memory and its share of the disk. a need not be running.
func (p Prices) Hour(a store.Agent) int64 {
	a.State, a.HasDisk = store.AgentRunning, true
	return p.Cost(measure(a, secondsPerHour))
}

// scale is q*m/d, rounded to the nearest. A month of a large disk times its
// price would overflow an int64.
func scale(q, m, d int64) int64 {
	n := new(big.Int).Mul(big.NewInt(q), big.NewInt(m))
	n.Add(n, big.NewInt(d/2))
	return n.Quo(n, big.NewInt(d)).Int64()
}

// measure is what machine a used in so many seconds: its CPU and memory
// while it runs, its disk while it exists. A machine still starting, such as
// a pod waiting for a node, is not charged for CPU and memory.
func measure(a store.Agent, seconds int64) store.MachineUse {
	cpu, mem, disk := a.Class.Size()
	var u store.MachineUse
	if a.HasDisk {
		u.DiskMiBSeconds = disk * seconds
	}
	if a.State == store.AgentRunning {
		u.CPUMilliSeconds, u.MemoryMiBSeconds = cpu*seconds, mem*seconds
	}
	return u
}

// Biller meters machines, sends their time to the gateway and asks it about
// credit.
type Biller struct {
	st     *store.Store
	gw     *gateway.Client
	prices Prices
	log    *slog.Logger

	mu     sync.Mutex
	credit map[string]answer
	warned map[string]time.Time
}

// answer is the gateway's word on one organisation's credit, and when it
// came.
type answer struct {
	at     time.Time
	credit gateway.Credit
}

func New(st *store.Store, gw *gateway.Client, prices Prices, log *slog.Logger) *Biller {
	if log == nil {
		log = slog.Default()
	}
	return &Biller{
		st: st, gw: gw, prices: prices, log: log,
		credit: map[string]answer{}, warned: map[string]time.Time{},
	}
}

// On reports whether machines are billed.
func (b *Biller) On() bool { return b != nil && b.prices.On() }

func (b *Biller) Prices() Prices { return b.prices }

// Meter adds the machine time since the last round. The leader runs it.
func (b *Biller) Meter(ctx context.Context, now time.Time) {
	if !b.On() {
		return
	}
	if _, err := b.st.Meter(ctx, now, maxGap, measure); err != nil {
		b.log.Warn("could not meter the machines", "error", err)
	}
}

// Send puts every complete hour not yet billed on the gateway's bill. The
// leader runs it. What fails is sent again next round.
func (b *Biller) Send(ctx context.Context, now time.Time) {
	if !b.On() {
		return
	}
	for ctx.Err() == nil {
		hours, err := b.st.UnbilledHours(ctx, now.Add(-maxAge), hoursPerCall)
		if err != nil {
			b.log.Warn("could not read the machine time to bill", "error", err)
			return
		}
		if len(hours) == 0 {
			return
		}
		var lines []gateway.BillLine
		for _, h := range hours {
			lines = append(lines, b.prices.lines(h)...)
		}
		if len(lines) > 0 {
			if err := b.gw.Bill(ctx, lines); err != nil {
				b.warn("bill", "could not send the machine time to Keera Gateway; it is sent again "+
					"next round", err)
				return
			}
		}
		if err := b.st.MarkBilled(ctx, hours); err != nil {
			// The gateway records each line once, so sending them again is
			// harmless.
			b.log.Warn("could not mark machine time as billed", "error", err)
			return
		}
		if len(hours) < hoursPerCall {
			return
		}
	}
}

// warn logs at most once every warnEvery per kind of trouble.
func (b *Biller) warn(kind, msg string, err error) {
	b.mu.Lock()
	last := b.warned[kind]
	due := time.Since(last) >= warnEvery
	if due {
		b.warned[kind] = time.Now()
	}
	b.mu.Unlock()
	if !due {
		return
	}
	b.log.Warn(msg, "error", err)
}

// Refused is the gateway's refusal as an error: an organisation that may not
// run machines now, in words for its people.
type Refused gateway.Refusal

func (e *Refused) Error() string { return e.Message }

// IsRefused reports whether err is a Refused.
func IsRefused(err error) bool {
	_, ok := errors.AsType[*Refused](err)
	return ok
}

// MayRun says whether an organisation may start a machine. Without billing,
// it always may. When the gateway cannot be asked, its last answer counts,
// and without one the machine starts: its time is still metered and billed.
func (b *Biller) MayRun(ctx context.Context, orgID string) error {
	c, ok := b.Credit(ctx, orgID)
	if ok && c.Refusal != nil {
		return (*Refused)(c.Refusal)
	}
	return nil
}

// Credit is the gateway's word on an organisation's credit, at most
// creditFresh old if the gateway answers. It is false without billing, or
// when the gateway has never answered for this organisation.
func (b *Biller) Credit(ctx context.Context, orgID string) (gateway.Credit, bool) {
	if !b.On() {
		return gateway.Credit{}, false
	}
	b.mu.Lock()
	last, ok := b.credit[orgID]
	b.mu.Unlock()
	if ok && time.Since(last.at) < creditFresh {
		return last.credit, true
	}
	c, err := b.gw.Credit(ctx, orgID)
	if err != nil {
		b.warn("credit", "could not ask Keera Gateway about an organisation's credit; "+
			"machines start as its last answer said", err)
		return last.credit, ok
	}
	b.mu.Lock()
	b.credit[orgID] = answer{at: time.Now(), credit: c}
	b.mu.Unlock()
	return c, true
}
