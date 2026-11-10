package metrics

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func render(r *Registry, s Snapshot) string {
	var b strings.Builder
	r.Write(&b, s)
	return b.String()
}

func TestWrite(t *testing.T) {
	r := New("v1.2.3")
	created := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	started := created.Add(20 * time.Second)
	finished := started.Add(90 * time.Second)
	r.TaskEnded("chat", "succeeded", created, &started, &finished)
	r.TaskEnded("board", "cancelled", created, nil, &finished)
	r.Delivery("chat", "ok")
	r.ToolCall("jira", Outcome(nil), 300*time.Millisecond)
	r.ToolCall("jira", Outcome(errors.New("no")), time.Second)
	r.Webhook("slack", "unverified")
	r.MachineStart("agent", "ok")
	r.FolderCheck("gdrive", "error")
	r.GatewayCall("GET /v1/service/users", "5xx", 2*time.Second)
	r.Round(time.Unix(1767261600, 0), 40*time.Millisecond)

	out := render(r, Snapshot{
		Machines: map[[2]string]int{{"agent", "running"}: 2, {"workspace", "suspended"}: 0},
		Queued:   3, Running: 2, OldestQueued: 90 * time.Second, Undelivered: 1,
	})
	for _, want := range []string{
		`keera_agents_build_info{version="v1.2.3"} 1`,
		`keera_agents_machines{kind="agent",state="running"} 2`,
		`keera_agents_machines{kind="workspace",state="suspended"} 0`,
		"keera_agents_tasks_queued 3",
		"keera_agents_tasks_running 2",
		"keera_agents_oldest_queued_task_seconds 90.000000",
		"keera_agents_results_undelivered 1",
		`keera_agents_tasks_total{source="chat",state="succeeded"} 1`,
		`keera_agents_tasks_total{source="board",state="cancelled"} 1`,
		`keera_agents_task_wait_seconds_bucket{source="chat",le="15"} 0`,
		`keera_agents_task_wait_seconds_bucket{source="chat",le="30"} 1`,
		`keera_agents_task_duration_seconds_bucket{source="chat",state="succeeded",le="60"} 0`,
		`keera_agents_task_duration_seconds_bucket{source="chat",state="succeeded",le="120"} 1`,
		`keera_agents_task_duration_seconds_count{source="chat",state="succeeded"} 1`,
		`keera_agents_task_duration_seconds_sum{source="chat",state="succeeded"} 90.000000`,
		`keera_agents_deliveries_total{source="chat",outcome="ok"} 1`,
		`keera_agents_tool_calls_total{kind="jira",outcome="ok"} 1`,
		`keera_agents_tool_calls_total{kind="jira",outcome="error"} 1`,
		`keera_agents_tool_call_duration_seconds_bucket{kind="jira",outcome="ok",le="0.25"} 0`,
		`keera_agents_tool_call_duration_seconds_bucket{kind="jira",outcome="ok",le="0.5"} 1`,
		`keera_agents_tool_call_duration_seconds_bucket{kind="jira",outcome="error",le="1"} 1`,
		`keera_agents_tool_call_duration_seconds_bucket{kind="jira",outcome="ok",le="+Inf"} 1`,
		`keera_agents_webhooks_total{kind="slack",outcome="unverified"} 1`,
		`keera_agents_machine_starts_total{kind="agent",outcome="ok"} 1`,
		`keera_agents_folder_checks_total{kind="gdrive",outcome="error"} 1`,
		`keera_agents_gateway_calls_total{op="GET /v1/service/users",outcome="5xx"} 1`,
		`keera_agents_gateway_call_duration_seconds_bucket{op="GET /v1/service/users",outcome="5xx",le="2.5"} 1`,
		"keera_agents_background_last_round_timestamp_seconds 1767261600",
		`keera_agents_background_round_duration_seconds_bucket{le="0.05"} 1`,
		`keera_agents_background_round_duration_seconds_count{} 1`,
	} {
		if !strings.Contains(out, want+"\n") {
			t.Errorf("missing %s", want)
		}
	}
	// A task that never started has no wait or duration.
	if strings.Contains(out, `source="board",le=`) {
		t.Error("a task that never ran was timed")
	}
	// Every family is declared, even with no series yet.
	if !strings.Contains(out, "# TYPE keera_agents_tasks_total counter\n") ||
		!strings.Contains(out, "# TYPE keera_agents_task_wait_seconds histogram\n") {
		t.Error("a family's TYPE line is missing")
	}
}

func TestNilRegistryRecordsNothing(t *testing.T) {
	var r *Registry
	now := time.Now()
	r.TaskEnded("chat", "failed", now, &now, &now)
	r.Delivery("chat", "ok")
	r.ToolCall("jira", "ok", time.Second)
	r.Webhook("slack", "accepted")
	r.MachineStart("agent", "ok")
	r.FolderCheck("gdrive", "ok")
}

func TestLabelValuesAreEscaped(t *testing.T) {
	r := New("v1")
	r.Webhook("a\"b\\c\nd", "ok")
	out := render(r, Snapshot{})
	if want := `kind="a\"b\\c\nd"`; !strings.Contains(out, want) {
		t.Errorf("want %s in\n%s", want, out)
	}
}

func TestNegativeDurationsCountAsZero(t *testing.T) {
	r := New("v1")
	created := time.Now()
	started := created.Add(-time.Second) // another replica's clock
	r.TaskEnded("chat", "succeeded", created, &started, &started)
	out := render(r, Snapshot{})
	if !strings.Contains(out, `keera_agents_task_wait_seconds_bucket{source="chat",le="1"} 1`) {
		t.Errorf("a negative wait was not counted as zero:\n%s", out)
	}
}
