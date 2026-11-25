package api

import (
	"fmt"
	"net/http"
	"time"

	"github.com/bespinian/keera-agents/internal/billing"
	"github.com/bespinian/keera-agents/internal/gateway"
	"github.com/bespinian/keera-agents/internal/store"
)

// machineCost is one machine's time in a month, and what it cost.
type machineCost struct {
	store.MachineMonth
	CPUHours       float64 `json:"cpu_hours"`
	MemoryGiBHours float64 `json:"memory_gib_hours"`
	DiskGiBMonths  float64 `json:"disk_gib_months"`
	CostMicros     int64   `json:"cost_micros"`
}

// billing is one month of the organisation's machine time, per machine. It
// is what keera-agents metered. The bill and the credit are in Keera
// Gateway. Administrators only, as in the gateway.
func (s *Server) billing(w http.ResponseWriter, r *http.Request, p Principal) error {
	if !p.CanAdmin() {
		return forbid("only an administrator can read what machines cost")
	}
	b := s.opts.Billing
	if !b.On() {
		return billingOff()
	}
	from, err := parseMonth(r.URL.Query().Get("month"), time.Now())
	if err != nil {
		return badRequest(err.Error())
	}
	to := from.AddDate(0, 1, 0)
	rows, err := s.st.MachineUsage(r.Context(), p.OrgID, from, to)
	if err != nil {
		return err
	}
	prices := b.Prices()
	machines := make([]machineCost, 0, len(rows))
	var total int64
	for _, m := range rows {
		c := machineCost{MachineMonth: m, CostMicros: prices.Cost(m.MachineUse)}
		c.CPUHours, c.MemoryGiBHours, c.DiskGiBMonths = billing.Quantities(m.MachineUse)
		total += c.CostMicros
		machines = append(machines, c)
	}
	var credit *gateway.Credit
	if c, ok := b.Credit(r.Context(), p.OrgID); ok {
		credit = &c
	}
	return writeJSON(w, http.StatusOK, map[string]any{
		"month": from.Format("2006-01"), "from": from, "to": to,
		"prices": prices, "machines": machines, "total_micros": total, "credit": credit,
	})
}

// agentCost is what one agent's machine cost this month, and what an hour of
// it running costs. Its owner and administrators see it, as they see the
// machine's settings.
func (s *Server) agentCost(w http.ResponseWriter, r *http.Request, p Principal) error {
	a, err := s.editableAgent(r, p, r.PathValue("id"))
	if err != nil {
		return err
	}
	return s.machineCost(w, r, a)
}

// workspaceCost is agentCost for a workspace, which only its owner and
// administrators see at all.
func (s *Server) workspaceCost(w http.ResponseWriter, r *http.Request, p Principal) error {
	a, err := s.workspaceRef(r, p)
	if err != nil {
		return err
	}
	return s.machineCost(w, r, a)
}

func (s *Server) machineCost(w http.ResponseWriter, r *http.Request, a store.Agent) error {
	b := s.opts.Billing
	if !b.On() {
		return billingOff()
	}
	from := monthStart(time.Now())
	used, err := s.st.AgentUsage(r.Context(), a.ID, from)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, agentCostView{
		Month:       from.Format("2006-01"),
		MonthMicros: b.Prices().Cost(used),
		HourMicros:  b.Prices().Hour(a),
	})
}

func billingOff() error {
	return refuse(http.StatusNotFound, "billing_off", "this deployment does not bill machine time")
}

// agentCostView is one machine's cost.
type agentCostView struct {
	Month       string `json:"month"`
	MonthMicros int64  `json:"month_micros"`
	// HourMicros is an hour running, at its size now: CPU, memory and its
	// share of the disk.
	HourMicros int64 `json:"hour_micros"`
}

// monthStart is the start of now's month, in UTC as the gateway's bill is.
func monthStart(now time.Time) time.Time {
	y, m, _ := now.UTC().Date()
	return time.Date(y, m, 1, 0, 0, 0, 0, time.UTC)
}

// parseMonth reads a month written as YYYY-MM, in UTC. Empty is now's.
func parseMonth(raw string, now time.Time) (time.Time, error) {
	if raw == "" {
		return monthStart(now), nil
	}
	t, err := time.Parse("2006-01", raw)
	if err != nil {
		return time.Time{}, fmt.Errorf("month is %q; write it as YYYY-MM, such as 2026-09", raw)
	}
	return t, nil
}
