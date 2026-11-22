package kubernetes

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/bespinian/keera-agents/internal/connectors"
	"github.com/bespinian/keera-agents/internal/connectors/internal/apitool"
	"github.com/bespinian/keera-agents/internal/connectors/internal/web"
)

const (
	// maxObject bounds what get returns, so a huge object does not fill the
	// model's context.
	maxObject = 64 << 10
	// maxLogBytes bounds what logs returns.
	maxLogBytes = 256 << 10
	// lastApplied is kubectl's copy of the last manifest applied. It can be
	// large, and for a Secret it holds the values.
	lastApplied = "kubectl.kubernetes.io/last-applied-configuration"
)

const kindArgs = `
	"api_version": {"type": "string", "description": "The apiVersion, such as v1 or apps/v1. Optional for common kinds."},
	"kind": {"type": "string", "description": "The kind, such as Pod, Deployment or ConfigMap."}`

func (a *Adapter) toolSet() apitool.Set {
	return apitool.Set{
		{
			Name:        "list",
			Description: "Lists objects of a kind in the namespace, with their age and a short status.",
			Parameters: apitool.Schema(`{"type": "object", "properties": {` + kindArgs + `,
				"label_selector": {"type": "string", "description": "Only objects with these labels, such as app=web."},
				"limit": {"type": "integer", "description": "How many to list, at most 100. Default 25."}
			}, "required": ["kind"]}`),
			Run: apitool.Typed(a.list),
		},
		{
			Name:        "get",
			Description: "Reads one object in the namespace as JSON. A Secret's values are left out; only its keys are shown.",
			Parameters: apitool.Schema(`{"type": "object", "properties": {` + kindArgs + `,
				"name": {"type": "string"}
			}, "required": ["kind", "name"]}`),
			Run: apitool.Typed(a.get),
		},
		{
			Name:        "logs",
			Description: "Reads the last lines a pod's container logged.",
			Parameters: apitool.Schema(`{"type": "object", "properties": {
				"pod": {"type": "string"},
				"container": {"type": "string", "description": "Needed when the pod has more than one container."},
				"tail_lines": {"type": "integer", "description": "How many lines from the end, at most 5000. Default 200."},
				"since_seconds": {"type": "integer", "description": "Only lines from the last this many seconds."},
				"previous": {"type": "boolean", "description": "The container's previous run, after a crash or restart."}
			}, "required": ["pod"]}`),
			Run: apitool.Typed(a.logs),
		},
		{
			Name:        "events",
			Description: "Lists the namespace's recent events, newest first, such as failed scheduling or image pulls.",
			Parameters: apitool.Schema(`{"type": "object", "properties": {
				"name": {"type": "string", "description": "Only events about the object with this name."},
				"limit": {"type": "integer", "description": "How many, at most 100. Default 25."}
			}}`),
			Run: apitool.Typed(a.events),
		},
		{
			Name: "apply",
			Description: "Creates or updates an object in the namespace with server-side apply. " +
				"Give the manifest as a JSON object; YAML is not accepted.",
			Parameters: apitool.Schema(`{"type": "object", "properties": {
				"manifest": {"type": "object", "description": "The whole object, with apiVersion, kind and metadata.name."}
			}, "required": ["manifest"]}`),
			Write: true,
			Run:   apitool.Typed(a.apply),
		},
		{
			Name:        "delete",
			Description: "Deletes one object in the namespace.",
			Parameters: apitool.Schema(`{"type": "object", "properties": {` + kindArgs + `,
				"name": {"type": "string"}
			}, "required": ["kind", "name"]}`),
			Write: true,
			Run:   apitool.Typed(a.delete),
		},
		{
			Name:        "scale",
			Description: "Sets how many replicas a Deployment or StatefulSet runs.",
			Parameters: apitool.Schema(`{"type": "object", "properties": {
				"kind": {"type": "string", "enum": ["Deployment", "StatefulSet"]},
				"name": {"type": "string"},
				"replicas": {"type": "integer", "minimum": 0, "maximum": 1000}
			}, "required": ["kind", "name", "replicas"]}`),
			Write: true,
			Run:   apitool.Typed(a.scale),
		},
		{
			Name:        "restart",
			Description: "Restarts a Deployment's, StatefulSet's or DaemonSet's pods one by one, as kubectl rollout restart does.",
			Parameters: apitool.Schema(`{"type": "object", "properties": {
				"kind": {"type": "string", "enum": ["Deployment", "StatefulSet", "DaemonSet"]},
				"name": {"type": "string"}
			}, "required": ["kind", "name"]}`),
			Write: true,
			Run:   apitool.Typed(a.restart),
		},
	}
}

// start opens the connection and checks the binding's namespace.
func (a *Adapter) start(conn connectors.Connection, target string) (cluster, string, error) {
	ns, err := namespace(target)
	if err != nil {
		return cluster{}, "", err
	}
	c, err := a.open(conn)
	return c, ns, err
}

type kindArgsT struct {
	APIVersion string `json:"api_version"`
	Kind       string `json:"kind"`
	Name       string `json:"name"`
}

// item is one object in a list.
type item struct {
	Name     string `json:"name"`
	Age      string `json:"age,omitempty"`
	Status   string `json:"status,omitempty"`
	Ready    string `json:"ready,omitempty"`
	Restarts int    `json:"restarts,omitempty"`
}

type listResult struct {
	Kind      string `json:"kind"`
	Namespace string `json:"namespace"`
	Items     []item `json:"items"`
	// More says the list was cut at the limit.
	More bool `json:"more,omitempty"`
}

func (a *Adapter) list(ctx context.Context, conn connectors.Connection, target string, args struct {
	kindArgsT
	LabelSelector string `json:"label_selector"`
	Limit         int    `json:"limit"`
},
) (any, error) {
	c, ns, err := a.start(conn, target)
	if err != nil {
		return nil, err
	}
	k, err := a.resolve(ctx, c, args.APIVersion, args.Kind, ns)
	if err != nil {
		return nil, err
	}
	q := url.Values{"limit": {strconv.Itoa(apitool.Limit(args.Limit, 25, 100))}}
	if args.LabelSelector != "" {
		q.Set("labelSelector", args.LabelSelector)
	}
	var l struct {
		Metadata struct {
			Continue string `json:"continue"`
		} `json:"metadata"`
		Items []map[string]any `json:"items"`
	}
	if err := a.call(ctx, c, http.MethodGet, k.path(ns, "")+"?"+q.Encode(), "", nil, &l); err != nil {
		return nil, err
	}
	out := listResult{Kind: k.Kind, Namespace: ns, Items: []item{}, More: l.Metadata.Continue != ""}
	now := time.Now()
	for _, o := range l.Items {
		out.Items = append(out.Items, summarize(k.Kind, o, now))
	}
	return out, nil
}

func (a *Adapter) get(ctx context.Context, conn connectors.Connection, target string, args kindArgsT) (any, error) {
	if err := checkName("name", args.Name); err != nil {
		return nil, err
	}
	c, ns, err := a.start(conn, target)
	if err != nil {
		return nil, err
	}
	k, err := a.resolve(ctx, c, args.APIVersion, args.Kind, ns)
	if err != nil {
		return nil, err
	}
	var o map[string]any
	if err := a.call(ctx, c, http.MethodGet, k.path(ns, args.Name), "", nil, &o); err != nil {
		return nil, err
	}
	clean(k.Kind, o)
	b, err := json.Marshal(o)
	if err != nil {
		return nil, err
	}
	if len(b) > maxObject {
		return web.Cut(string(b), maxObject, fmt.Sprintf("\n… cut: the object is %d KiB", len(b)>>10)), nil
	}
	return json.RawMessage(b), nil
}

// clean drops what the model does not need from an object, and every value
// of a Secret.
func clean(kind string, o map[string]any) {
	if md, ok := o["metadata"].(map[string]any); ok {
		delete(md, "managedFields")
		if an, ok := md["annotations"].(map[string]any); ok {
			delete(an, lastApplied)
		}
	}
	if kind != "Secret" {
		return
	}
	keys := []string{}
	for _, f := range []string{"data", "stringData"} {
		if m, ok := o[f].(map[string]any); ok {
			for k := range m {
				keys = append(keys, k)
			}
		}
		delete(o, f)
	}
	slices.Sort(keys)
	o["data_keys"] = slices.Compact(keys)
}

func (a *Adapter) logs(ctx context.Context, conn connectors.Connection, target string, args struct {
	Pod          string `json:"pod"`
	Container    string `json:"container"`
	TailLines    int    `json:"tail_lines"`
	SinceSeconds int    `json:"since_seconds"`
	Previous     bool   `json:"previous"`
},
) (any, error) {
	if err := checkName("pod", args.Pod); err != nil {
		return nil, err
	}
	c, ns, err := a.start(conn, target)
	if err != nil {
		return nil, err
	}
	q := url.Values{
		"tailLines":  {strconv.Itoa(apitool.Limit(args.TailLines, 200, 5000))},
		"limitBytes": {strconv.Itoa(maxLogBytes)},
	}
	if args.Container != "" {
		if err := checkName("container", args.Container); err != nil {
			return nil, err
		}
		q.Set("container", args.Container)
	}
	if args.SinceSeconds > 0 {
		q.Set("sinceSeconds", strconv.Itoa(args.SinceSeconds))
	}
	if args.Previous {
		q.Set("previous", "true")
	}
	b, err := a.do(ctx, c, http.MethodGet, "/api/v1/namespaces/"+ns+"/pods/"+url.PathEscape(args.Pod)+"/log?"+q.Encode(), "", nil, maxLogBytes+4096)
	if err != nil {
		return nil, err
	}
	if len(b) == 0 {
		return "(no log lines)", nil
	}
	return strings.ToValidUTF8(string(b), "�"), nil
}

type event struct {
	Time    string `json:"time"`
	Type    string `json:"type"`
	Reason  string `json:"reason"`
	Object  string `json:"object"`
	Message string `json:"message"`
	Count   int    `json:"count,omitempty"`
}

func (a *Adapter) events(ctx context.Context, conn connectors.Connection, target string, args struct {
	Name  string `json:"name"`
	Limit int    `json:"limit"`
},
) (any, error) {
	c, ns, err := a.start(conn, target)
	if err != nil {
		return nil, err
	}
	path := "/api/v1/namespaces/" + ns + "/events"
	if args.Name != "" {
		if err := checkName("name", args.Name); err != nil {
			return nil, err
		}
		path += "?" + url.Values{"fieldSelector": {"involvedObject.name=" + args.Name}}.Encode()
	}
	var l struct {
		Items []struct {
			Metadata struct {
				CreationTimestamp string `json:"creationTimestamp"`
			} `json:"metadata"`
			InvolvedObject struct {
				Kind string `json:"kind"`
				Name string `json:"name"`
			} `json:"involvedObject"`
			Type          string `json:"type"`
			Reason        string `json:"reason"`
			Message       string `json:"message"`
			Count         int    `json:"count"`
			LastTimestamp string `json:"lastTimestamp"`
			EventTime     string `json:"eventTime"`
			Series        struct {
				Count            int    `json:"count"`
				LastObservedTime string `json:"lastObservedTime"`
			} `json:"series"`
		} `json:"items"`
	}
	if err := a.call(ctx, c, http.MethodGet, path, "", nil, &l); err != nil {
		return nil, err
	}
	out := []event{}
	for _, e := range l.Items {
		// Newer events set eventTime, at the first time, and series when
		// they happen again; older ones lastTimestamp and count.
		at := cmp.Or(e.Series.LastObservedTime, e.LastTimestamp, e.EventTime, e.Metadata.CreationTimestamp)
		out = append(out, event{
			Time: at, Type: e.Type, Reason: e.Reason, Object: e.InvolvedObject.Kind + "/" + e.InvolvedObject.Name,
			Message: web.Cut(e.Message, 500, "…"), Count: cmp.Or(e.Series.Count, e.Count),
		})
	}
	// RFC 3339 times in UTC sort as strings, and Kubernetes writes them so.
	slices.SortStableFunc(out, func(x, y event) int { return strings.Compare(y.Time, x.Time) })
	return out[:min(len(out), apitool.Limit(args.Limit, 25, 100))], nil
}

type changed struct {
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	Done      string `json:"done"`
	Status    *item  `json:"status,omitempty"`
}

func (a *Adapter) apply(ctx context.Context, conn connectors.Connection, target string, args struct {
	Manifest json.RawMessage `json:"manifest"`
},
) (any, error) {
	var m map[string]any
	if s := strings.TrimSpace(string(args.Manifest)); !strings.HasPrefix(s, "{") || json.Unmarshal(args.Manifest, &m) != nil {
		return nil, errors.New("give the manifest as a JSON object; YAML is not accepted")
	}
	apiVersion, _ := m["apiVersion"].(string)
	kind, _ := m["kind"].(string)
	md, _ := m["metadata"].(map[string]any)
	name, _ := md["name"].(string)
	if apiVersion == "" || md == nil {
		return nil, errors.New("the manifest needs apiVersion, kind and metadata.name")
	}
	if err := checkName("metadata.name", name); err != nil {
		return nil, err
	}
	c, ns, err := a.start(conn, target)
	if err != nil {
		return nil, err
	}
	if other, _ := md["namespace"].(string); other != "" && other != ns {
		return nil, fmt.Errorf("the manifest names namespace %s, but this connector works only within %s", web.Short(other, 63), ns)
	}
	k, err := a.resolve(ctx, c, apiVersion, kind, ns)
	if err != nil {
		return nil, err
	}
	md["namespace"], m["kind"] = ns, k.Kind
	q := url.Values{"fieldManager": {"keera-agents"}, "force": {"true"}}
	var o map[string]any
	// Server-side apply takes YAML, and JSON is YAML.
	if err := a.call(ctx, c, http.MethodPatch, k.path(ns, name)+"?"+q.Encode(), "application/apply-patch+yaml", m, &o); err != nil {
		return nil, err
	}
	// Only a summary goes back, so a Secret's values are never echoed.
	s := summarize(k.Kind, o, time.Now())
	return changed{Kind: k.Kind, Name: name, Namespace: ns, Done: "applied", Status: &s}, nil
}

func (a *Adapter) delete(ctx context.Context, conn connectors.Connection, target string, args kindArgsT) (any, error) {
	if err := checkName("name", args.Name); err != nil {
		return nil, err
	}
	c, ns, err := a.start(conn, target)
	if err != nil {
		return nil, err
	}
	k, err := a.resolve(ctx, c, args.APIVersion, args.Kind, ns)
	if err != nil {
		return nil, err
	}
	// Background deletes what the object owns too, such as a Deployment's
	// pods, as kubectl does.
	if err := a.call(ctx, c, http.MethodDelete, k.path(ns, args.Name)+"?propagationPolicy=Background", "", nil, nil); err != nil {
		return nil, err
	}
	return changed{Kind: k.Kind, Name: args.Name, Namespace: ns, Done: "deleted"}, nil
}

// workloads are the apps/v1 kinds scale and restart take, by lower-case kind.
var workloads = map[string]kindRef{
	"deployment":  {apiVersion: "apps/v1", prefix: "/apis/apps/v1", resource: resource{Name: "deployments", Kind: "Deployment", Namespaced: true}},
	"statefulset": {apiVersion: "apps/v1", prefix: "/apis/apps/v1", resource: resource{Name: "statefulsets", Kind: "StatefulSet", Namespaced: true}},
	"daemonset":   {apiVersion: "apps/v1", prefix: "/apis/apps/v1", resource: resource{Name: "daemonsets", Kind: "DaemonSet", Namespaced: true}},
}

func workload(kind string, allowed ...string) (kindRef, error) {
	k := strings.ToLower(strings.TrimSpace(kind))
	if !slices.Contains(allowed, k) {
		return kindRef{}, fmt.Errorf("kind must be one of %s", strings.Join(allowed, ", "))
	}
	return workloads[k], nil
}

func (a *Adapter) scale(ctx context.Context, conn connectors.Connection, target string, args struct {
	Kind     string `json:"kind"`
	Name     string `json:"name"`
	Replicas *int   `json:"replicas"`
},
) (any, error) {
	k, err := workload(args.Kind, "deployment", "statefulset")
	if err != nil {
		return nil, err
	}
	if err := checkName("name", args.Name); err != nil {
		return nil, err
	}
	if args.Replicas == nil || *args.Replicas < 0 || *args.Replicas > 1000 {
		return nil, errors.New("give replicas, from 0 to 1000")
	}
	c, ns, err := a.start(conn, target)
	if err != nil {
		return nil, err
	}
	patch := map[string]any{"spec": map[string]any{"replicas": *args.Replicas}}
	if err := a.call(ctx, c, http.MethodPatch, k.path(ns, args.Name)+"/scale", "application/merge-patch+json", patch, nil); err != nil {
		return nil, err
	}
	return changed{Kind: k.Kind, Name: args.Name, Namespace: ns, Done: fmt.Sprintf("scaled to %d replicas", *args.Replicas)}, nil
}

func (a *Adapter) restart(ctx context.Context, conn connectors.Connection, target string, args kindArgsT) (any, error) {
	k, err := workload(args.Kind, "deployment", "statefulset", "daemonset")
	if err != nil {
		return nil, err
	}
	if err := checkName("name", args.Name); err != nil {
		return nil, err
	}
	c, ns, err := a.start(conn, target)
	if err != nil {
		return nil, err
	}
	// A changed pod template rolls the pods, which is all kubectl rollout
	// restart does.
	patch := map[string]any{"spec": map[string]any{"template": map[string]any{"metadata": map[string]any{
		"annotations": map[string]string{"kubectl.kubernetes.io/restartedAt": time.Now().UTC().Format(time.RFC3339)},
	}}}}
	if err := a.call(ctx, c, http.MethodPatch, k.path(ns, args.Name), "application/merge-patch+json", patch, nil); err != nil {
		return nil, err
	}
	return changed{Kind: k.Kind, Name: args.Name, Namespace: ns, Done: "restarting"}, nil
}

// summarize is an object's line in a list: what kubectl get shows.
func summarize(kind string, o map[string]any, now time.Time) item {
	it := item{Name: str(o, "metadata", "name"), Age: age(str(o, "metadata", "creationTimestamp"), now)}
	switch kind {
	case "Pod":
		it.Status = str(o, "status", "phase")
		statuses, _ := field(o, "status", "containerStatuses").([]any)
		ready := 0
		for _, s := range statuses {
			cs, _ := s.(map[string]any)
			if b, _ := cs["ready"].(bool); b {
				ready++
			}
			it.Restarts += num(cs, "restartCount")
			// A waiting container's reason, such as CrashLoopBackOff, says
			// more than the pod's phase.
			if r := str(cs, "state", "waiting", "reason"); r != "" {
				it.Status = r
			}
		}
		containers, _ := field(o, "spec", "containers").([]any)
		it.Ready = fmt.Sprintf("%d/%d", ready, max(len(containers), len(statuses)))
	case "Deployment", "StatefulSet", "ReplicaSet":
		it.Ready = fmt.Sprintf("%d/%d", num(o, "status", "readyReplicas"), num(o, "spec", "replicas"))
	case "DaemonSet":
		it.Ready = fmt.Sprintf("%d/%d", num(o, "status", "numberReady"), num(o, "status", "desiredNumberScheduled"))
	case "Job":
		it.Status = fmt.Sprintf("%d succeeded, %d failed, %d active",
			num(o, "status", "succeeded"), num(o, "status", "failed"), num(o, "status", "active"))
	case "CronJob":
		it.Status = "schedule " + str(o, "spec", "schedule")
		if b, _ := field(o, "spec", "suspend").(bool); b {
			it.Status += ", suspended"
		}
	case "Service":
		it.Status = strings.TrimSpace(str(o, "spec", "type") + " " + str(o, "spec", "clusterIP"))
	case "Secret":
		it.Status = str(o, "type")
	default:
		it.Status = str(o, "status", "phase")
		if it.Status == "" {
			it.Status = readyCondition(o)
		}
	}
	if _, ok := field(o, "metadata", "deletionTimestamp").(string); ok {
		it.Status = "Terminating"
	}
	return it
}

// readyCondition is the Ready or Available condition, as most kinds with
// a status report it.
func readyCondition(o map[string]any) string {
	conds, _ := field(o, "status", "conditions").([]any)
	for _, c := range conds {
		m, _ := c.(map[string]any)
		if t := str(m, "type"); t == "Ready" || t == "Available" {
			return t + "=" + str(m, "status")
		}
	}
	return ""
}

func field(o map[string]any, path ...string) any {
	var v any = o
	for _, p := range path {
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = m[p]
	}
	return v
}

func str(o map[string]any, path ...string) string {
	s, _ := field(o, path...).(string)
	return s
}

func num(o map[string]any, path ...string) int {
	f, _ := field(o, path...).(float64)
	return int(f)
}

// age is how long ago ts was, as kubectl shows it.
func age(ts string, now time.Time) string {
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return ""
	}
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", max(int(d.Seconds()), 0))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}
