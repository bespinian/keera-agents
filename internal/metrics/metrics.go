// Package metrics is a small hand-written Prometheus exposition, as Keera
// Gateway's is, to avoid the client library.
//
// Counters live in each replica and count what that replica did, so sum them
// across replicas. The gauges are read from the database at each scrape, so
// every replica reports the same values: take the max, not the sum.
package metrics

import (
	"bytes"
	"fmt"
	"io"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Buckets are in seconds. A task may run for the task timeout, half an hour
// by default, and wait in the queue for an hour before it fails.
var (
	taskBuckets = []float64{5, 15, 30, 60, 120, 300, 600, 900, 1800, 3600}
	waitBuckets = []float64{1, 5, 15, 30, 60, 120, 300, 600, 1800, 3600}
	callBuckets = []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120}
)

// Registry holds keera-agents' counters. A nil Registry records nothing, so
// tests need not make one.
type Registry struct {
	version      string
	tasks        *family
	taskDuration *family
	taskWait     *family
	deliveries   *family
	toolCalls    *family
	toolDuration *family
	webhooks     *family
	starts       *family
	folderChecks *family
}

// New makes a registry for the given build version.
func New(version string) *Registry {
	return &Registry{
		version: version,
		tasks: counter("keera_agents_tasks_total",
			"Finished tasks, by where they came from and how they ended.", "source", "state"),
		taskDuration: histogram("keera_agents_task_duration_seconds",
			"How long a task ran, from its start to its end.", taskBuckets, "source", "state"),
		taskWait: histogram("keera_agents_task_wait_seconds",
			"How long a task waited in the queue before it started.", waitBuckets, "source"),
		deliveries: counter("keera_agents_deliveries_total",
			"Results handed back to where their task came from. retry means nothing was sent and it is tried again.",
			"source", "outcome"),
		toolCalls: counter("keera_agents_tool_calls_total",
			"Platform and connector tool calls agents made, by connector kind or tool group.", "kind", "outcome"),
		toolDuration: histogram("keera_agents_tool_call_duration_seconds",
			"Wall time of a platform or connector tool call.", callBuckets, "kind", "outcome"),
		webhooks: counter("keera_agents_webhooks_total",
			"Webhooks from chat services, by what became of them.", "kind", "outcome"),
		starts: counter("keera_agents_machine_starts_total",
			"Containers created for agents and workspaces.", "kind", "outcome"),
		folderChecks: counter("keera_agents_folder_checks_total",
			"Checks of watched folders for new files.", "kind", "outcome"),
	}
}

// Outcome is "ok" for a nil error and "error" otherwise.
func Outcome(err error) string {
	if err != nil {
		return "error"
	}
	return "ok"
}

// TaskEnded records a task whose result was handed back. started is nil for
// a task that never ran, as one cancelled while it waited.
func (r *Registry) TaskEnded(source, state string, created time.Time, started, finished *time.Time) {
	if r == nil {
		return
	}
	r.tasks.inc(source, state)
	if started == nil {
		return
	}
	r.taskWait.observe(started.Sub(created), source)
	if finished != nil {
		r.taskDuration.observe(finished.Sub(*started), source, state)
	}
}

// Delivery records one attempt to hand a result back.
func (r *Registry) Delivery(source, outcome string) {
	if r != nil {
		r.deliveries.inc(source, outcome)
	}
}

// ToolCall records one platform or connector tool call.
func (r *Registry) ToolCall(kind, outcome string, d time.Duration) {
	if r == nil {
		return
	}
	r.toolCalls.inc(kind, outcome)
	r.toolDuration.observe(d, kind, outcome)
}

// Webhook records one webhook for a known connection.
func (r *Registry) Webhook(kind, outcome string) {
	if r != nil {
		r.webhooks.inc(kind, outcome)
	}
}

// MachineStart records one attempt to create a container.
func (r *Registry) MachineStart(kind, outcome string) {
	if r != nil {
		r.starts.inc(kind, outcome)
	}
}

// FolderCheck records one check of a watched folder.
func (r *Registry) FolderCheck(kind, outcome string) {
	if r != nil {
		r.folderChecks.inc(kind, outcome)
	}
}

// Snapshot is what the database says at the time of a scrape.
type Snapshot struct {
	// Machines counts agents and workspaces by kind and state.
	Machines map[[2]string]int
	Queued   int
	Running  int
	// OldestQueued is how long the oldest queued task has waited.
	OldestQueued time.Duration
	// Undelivered counts finished tasks whose result has not gone back yet.
	Undelivered int
}

// Write renders the exposition format. It renders into memory first, so a
// slow reader never holds a lock that tool calls wait on.
func (r *Registry) Write(w io.Writer, s Snapshot) {
	var buf bytes.Buffer
	header(&buf, "keera_agents_build_info", "The version keera-agents was built as.", "gauge")
	fmt.Fprintf(&buf, "keera_agents_build_info{version=%s} 1\n", quote(r.version))

	header(&buf, "keera_agents_machines", "Agents and workspaces, by kind and state.", "gauge")
	for _, k := range slices.SortedFunc(maps.Keys(s.Machines), func(a, b [2]string) int {
		return strings.Compare(a[0]+"\x00"+a[1], b[0]+"\x00"+b[1])
	}) {
		fmt.Fprintf(&buf, "keera_agents_machines{kind=%s,state=%s} %d\n", quote(k[0]), quote(k[1]), s.Machines[k])
	}
	gauge(&buf, "keera_agents_tasks_queued", "Tasks waiting for their agent.", s.Queued)
	gauge(&buf, "keera_agents_tasks_running", "Tasks an agent is working on.", s.Running)
	header(&buf, "keera_agents_oldest_queued_task_seconds",
		"How long the oldest queued task has waited; 0 with none queued.", "gauge")
	fmt.Fprintf(&buf, "keera_agents_oldest_queued_task_seconds %s\n", seconds(s.OldestQueued))
	gauge(&buf, "keera_agents_results_undelivered",
		"Finished tasks whose result has not gone back yet.", s.Undelivered)

	for _, f := range []*family{
		r.tasks, r.taskDuration, r.taskWait, r.deliveries, r.toolCalls, r.toolDuration,
		r.webhooks, r.starts, r.folderChecks,
	} {
		f.write(&buf)
	}
	_, _ = w.Write(buf.Bytes())
}

// family is one metric and its series, keyed by label values joined by NUL.
type family struct {
	name, help, typ string
	labels          []string
	buckets         []float64

	mu     sync.RWMutex
	series map[string]*series
}

type series struct {
	// count is a counter's value. A histogram counts in hist.
	count atomic.Int64
	// micros is a histogram's sum, in microseconds so it stays an integer.
	micros atomic.Int64
	hist   []atomic.Int64
}

func counter(name, help string, labels ...string) *family {
	return &family{name: name, help: help, typ: "counter", labels: labels, series: map[string]*series{}}
}

func histogram(name, help string, buckets []float64, labels ...string) *family {
	return &family{
		name: name, help: help, typ: "histogram", labels: labels, buckets: buckets,
		series: map[string]*series{},
	}
}

// with finds the series for one set of label values. It takes the write lock
// only for a new set.
func (f *family) with(values ...string) *series {
	key := strings.Join(values, "\x00")
	f.mu.RLock()
	s, ok := f.series[key]
	f.mu.RUnlock()
	if ok {
		return s
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if s, ok = f.series[key]; ok {
		return s
	}
	s = &series{hist: make([]atomic.Int64, len(f.buckets)+1)}
	f.series[key] = s
	return s
}

func (f *family) inc(values ...string) { f.with(values...).count.Add(1) }

func (f *family) observe(d time.Duration, values ...string) {
	d = max(d, 0) // clocks of two replicas may disagree
	s := f.with(values...)
	s.micros.Add(d.Microseconds())
	i, _ := slices.BinarySearch(f.buckets, d.Seconds())
	s.hist[i].Add(1)
}

func (f *family) write(w *bytes.Buffer) {
	header(w, f.name, f.help, f.typ)
	f.mu.RLock()
	defer f.mu.RUnlock()
	for _, key := range slices.Sorted(maps.Keys(f.series)) {
		s, labels := f.series[key], f.labelSet(key)
		if f.typ == "counter" {
			fmt.Fprintf(w, "%s{%s} %d\n", f.name, labels, s.count.Load())
			continue
		}
		var cum int64
		for i, ub := range f.buckets {
			cum += s.hist[i].Load()
			fmt.Fprintf(w, "%s_bucket{%s,le=\"%s\"} %d\n",
				f.name, labels, strconv.FormatFloat(ub, 'g', -1, 64), cum)
		}
		cum += s.hist[len(f.buckets)].Load()
		fmt.Fprintf(w, "%s_bucket{%s,le=\"+Inf\"} %d\n", f.name, labels, cum)
		fmt.Fprintf(w, "%s_count{%s} %d\n", f.name, labels, cum)
		fmt.Fprintf(w, "%s_sum{%s} %s\n", f.name, labels, seconds(time.Duration(s.micros.Load())*time.Microsecond))
	}
}

// labelSet renders a NUL-joined key as name="value" pairs.
func (f *family) labelSet(key string) string {
	values := strings.SplitN(key, "\x00", len(f.labels))
	pairs := make([]string, len(f.labels))
	for i, name := range f.labels {
		v := ""
		if i < len(values) {
			v = values[i]
		}
		pairs[i] = name + "=" + quote(v)
	}
	return strings.Join(pairs, ",")
}

func header(w *bytes.Buffer, name, help, typ string) {
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, typ)
}

func gauge(w *bytes.Buffer, name, help string, v int) {
	header(w, name, help, "gauge")
	fmt.Fprintf(w, "%s %d\n", name, v)
}

func seconds(d time.Duration) string { return strconv.FormatFloat(d.Seconds(), 'f', 6, 64) }

// escaper escapes a label value as the exposition format wants: only
// backslash, double quote and newline.
var escaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)

func quote(v string) string { return `"` + escaper.Replace(v) + `"` }
