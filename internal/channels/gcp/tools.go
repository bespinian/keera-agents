package gcp

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/bespinian/keera-agents/internal/channels"
	"github.com/bespinian/keera-agents/internal/channels/internal/apitool"
	"github.com/bespinian/keera-agents/internal/channels/internal/web"
)

const (
	// maxPayload bounds one log entry's text.
	maxPayload = 2000
	// maxPoints bounds one time series' points.
	maxPoints = 120
	// maxMinutes is how far back logs and metrics look, at most: a week.
	maxMinutes = 7 * 24 * 60
)

var (
	validZone     = regexp.MustCompile(`^[a-z]+-[a-z]+[0-9]+-[a-z]$`)
	validInstance = regexp.MustCompile(`^[a-z]([-a-z0-9]{0,61}[a-z0-9])?$`)
	validAligner  = regexp.MustCompile(`^ALIGN_[A-Z0-9_]+$`)
	validType     = regexp.MustCompile(`^[A-Za-z0-9.]+/[A-Za-z0-9.]+$`)
)

const sinceArg = `"since_minutes": {"type": "integer", "description": "How far back to look, in minutes, at most a week. Default 60."}`

func (a *Adapter) toolSet() apitool.Set {
	return apitool.Set{
		{
			Name:        "search_resources",
			Description: "Searches the project's resources of every kind, such as VMs, buckets, clusters and databases, with Cloud Asset Inventory.",
			Parameters: apitool.Schema(`{"type": "object", "properties": {
				"query": {"type": "string", "description": "Cloud Asset search query, such as name:web or labels.env:prod or state:RUNNING. Empty finds all."},
				"asset_types": {"type": "array", "items": {"type": "string"}, "description": "Only these types, such as compute.googleapis.com/Instance."},
				"limit": {"type": "integer", "description": "How many, at most 100. Default 25."}
			}}`),
			Run: a.searchResources,
		},
		{
			Name:        "list_instances",
			Description: "Lists the project's Compute Engine VMs in every zone, with status, machine type and IP addresses.",
			Parameters: apitool.Schema(`{"type": "object", "properties": {
				"filter": {"type": "string", "description": "Compute Engine filter, such as status = RUNNING or labels.env = prod."},
				"limit": {"type": "integer", "description": "How many, at most 100. Default 25."}
			}}`),
			Run: a.listInstances,
		},
		{
			Name:        "logs",
			Description: "Reads the project's log entries from Cloud Logging, newest first.",
			Parameters: apitool.Schema(`{"type": "object", "properties": {
				"filter": {"type": "string", "description": "Logging query, such as resource.type=\"k8s_container\" AND severity>=ERROR."},
				` + sinceArg + `,
				"limit": {"type": "integer", "description": "How many entries, at most 100. Default 25."}
			}}`),
			Run: a.logs,
		},
		{
			Name:        "metrics",
			Description: "Reads time series from Cloud Monitoring, newest points first.",
			Parameters: apitool.Schema(`{"type": "object", "properties": {
				"filter": {"type": "string", "description": "Monitoring filter naming one metric type, such as metric.type = \"compute.googleapis.com/instance/cpu/utilization\"."},
				` + sinceArg + `,
				"alignment_period_seconds": {"type": "integer", "description": "Combine each series' points into one per this many seconds, such as 300."},
				"aligner": {"type": "string", "description": "How points are combined, such as ALIGN_MEAN (the default), ALIGN_MAX or ALIGN_RATE for counters."},
				"limit": {"type": "integer", "description": "How many series, at most 100. Default 25."}
			}, "required": ["filter"]}`),
			Run: a.metrics,
		},
		{
			Name:        "list_buckets",
			Description: "Lists the project's Cloud Storage buckets.",
			Parameters: apitool.Schema(`{"type": "object", "properties": {
				"limit": {"type": "integer", "description": "How many, at most 100. Default 25."}
			}}`),
			Run: a.listBuckets,
		},
		{
			Name:        "list_objects",
			Description: "Lists the objects in a bucket of the project, one folder level at a time.",
			Parameters: apitool.Schema(`{"type": "object", "properties": {
				"bucket": {"type": "string"},
				"prefix": {"type": "string", "description": "Only objects whose names start with this, such as reports/2024/."},
				"limit": {"type": "integer", "description": "How many, at most 100. Default 25."}
			}, "required": ["bucket"]}`),
			Run: a.listObjects,
		},
		{
			Name:        "read_object",
			Description: "Reads a text object in a bucket of the project, 48 KiB at a time. For the rest, read again from next_offset.",
			Parameters: apitool.Schema(`{"type": "object", "properties": {
				"bucket": {"type": "string"},
				"name": {"type": "string", "description": "The object's whole name, such as reports/2024/q1.csv."},
				"offset": {"type": "integer", "description": "The byte to start from: 0, or an earlier read's next_offset."}
			}, "required": ["bucket", "name"]}`),
			Run: a.readObject,
		},
		{
			Name:        "instance_start",
			Description: "Starts a stopped Compute Engine VM.",
			Parameters:  apitool.Schema(instanceArgs),
			Write:       true,
			Run:         a.instanceAction("start"),
		},
		{
			Name:        "instance_stop",
			Description: "Stops a running Compute Engine VM.",
			Parameters:  apitool.Schema(instanceArgs),
			Write:       true,
			Run:         a.instanceAction("stop"),
		},
	}
}

const instanceArgs = `{"type": "object", "properties": {
	"zone": {"type": "string", "description": "Such as europe-west6-a."},
	"name": {"type": "string"}
}, "required": ["zone", "name"]}`

type resource struct {
	Name        string            `json:"name"`
	AssetType   string            `json:"asset_type"`
	DisplayName string            `json:"display_name,omitempty"`
	Location    string            `json:"location,omitempty"`
	State       string            `json:"state,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
}

func (a *Adapter) searchResources(ctx context.Context, conn channels.Connection, target string, raw json.RawMessage) (any, error) {
	args, err := apitool.Decode[struct {
		Query      string   `json:"query"`
		AssetTypes []string `json:"asset_types"`
		Limit      int      `json:"limit"`
	}](raw)
	if err != nil {
		return nil, err
	}
	id, err := project(target)
	if err != nil {
		return nil, err
	}
	q := url.Values{"pageSize": {strconv.Itoa(apitool.Limit(args.Limit, 25, 100))}}
	if args.Query != "" {
		q.Set("query", args.Query)
	}
	for _, t := range args.AssetTypes {
		if !validType.MatchString(t) {
			return nil, fmt.Errorf("%q is not an asset type; write service/Kind, such as compute.googleapis.com/Instance", web.Short(t, 80))
		}
		q.Add("assetTypes", t)
	}
	var r struct {
		Results []struct {
			Name        string            `json:"name"`
			AssetType   string            `json:"assetType"`
			DisplayName string            `json:"displayName"`
			Location    string            `json:"location"`
			State       string            `json:"state"`
			Labels      map[string]string `json:"labels"`
		} `json:"results"`
	}
	// The scope in the path is what keeps the search in the project.
	if err := a.call(ctx, conn, http.MethodGet, a.asset+"/v1/projects/"+id+":searchAllResources?"+q.Encode(), nil, &r); err != nil {
		return nil, err
	}
	out := []resource{}
	for _, x := range r.Results {
		out = append(out, resource{
			Name: strings.TrimPrefix(x.Name, "//"), AssetType: x.AssetType, DisplayName: x.DisplayName,
			Location: x.Location, State: x.State, Labels: x.Labels,
		})
	}
	return out, nil
}

type instance struct {
	Name        string            `json:"name"`
	Zone        string            `json:"zone"`
	Status      string            `json:"status"`
	MachineType string            `json:"machine_type"`
	InternalIPs []string          `json:"internal_ips,omitempty"`
	ExternalIPs []string          `json:"external_ips,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
}

func (a *Adapter) listInstances(ctx context.Context, conn channels.Connection, target string, raw json.RawMessage) (any, error) {
	args, err := apitool.Decode[struct {
		Filter string `json:"filter"`
		Limit  int    `json:"limit"`
	}](raw)
	if err != nil {
		return nil, err
	}
	id, err := project(target)
	if err != nil {
		return nil, err
	}
	limit := apitool.Limit(args.Limit, 25, 100)
	// Partial success lists the zones that answered even when one is down.
	q := url.Values{"maxResults": {strconv.Itoa(limit)}, "returnPartialSuccess": {"true"}}
	if args.Filter != "" {
		q.Set("filter", args.Filter)
	}
	var r struct {
		Items map[string]struct {
			Instances []struct {
				Name              string            `json:"name"`
				Zone              string            `json:"zone"`
				Status            string            `json:"status"`
				MachineType       string            `json:"machineType"`
				Labels            map[string]string `json:"labels"`
				NetworkInterfaces []struct {
					NetworkIP     string `json:"networkIP"`
					AccessConfigs []struct {
						NatIP string `json:"natIP"`
					} `json:"accessConfigs"`
				} `json:"networkInterfaces"`
			} `json:"instances"`
		} `json:"items"`
	}
	if err := a.call(ctx, conn, http.MethodGet, a.compute+"/compute/v1/projects/"+id+"/aggregated/instances?"+q.Encode(), nil, &r); err != nil {
		return nil, err
	}
	out := []instance{}
	for _, scoped := range r.Items {
		for _, x := range scoped.Instances {
			in := instance{Name: x.Name, Zone: path.Base(x.Zone), Status: x.Status, MachineType: path.Base(x.MachineType), Labels: x.Labels}
			for _, n := range x.NetworkInterfaces {
				if n.NetworkIP != "" {
					in.InternalIPs = append(in.InternalIPs, n.NetworkIP)
				}
				for _, ac := range n.AccessConfigs {
					if ac.NatIP != "" {
						in.ExternalIPs = append(in.ExternalIPs, ac.NatIP)
					}
				}
			}
			out = append(out, in)
		}
	}
	slices.SortFunc(out, func(x, y instance) int {
		return cmp.Or(strings.Compare(x.Zone, y.Zone), strings.Compare(x.Name, y.Name))
	})
	return out[:min(len(out), limit)], nil
}

// since is the start of a look back of minutes, by default an hour.
func since(minutes int, now time.Time) string {
	m := apitool.Limit(minutes, 60, maxMinutes)
	return now.Add(-time.Duration(m) * time.Minute).UTC().Format(time.RFC3339)
}

type logEntry struct {
	Time     string            `json:"time"`
	Severity string            `json:"severity,omitempty"`
	Log      string            `json:"log,omitempty"`
	Resource string            `json:"resource,omitempty"`
	Labels   map[string]string `json:"resource_labels,omitempty"`
	Text     string            `json:"text,omitempty"`
}

func (a *Adapter) logs(ctx context.Context, conn channels.Connection, target string, raw json.RawMessage) (any, error) {
	args, err := apitool.Decode[struct {
		Filter       string `json:"filter"`
		SinceMinutes int    `json:"since_minutes"`
		Limit        int    `json:"limit"`
	}](raw)
	if err != nil {
		return nil, err
	}
	id, err := project(target)
	if err != nil {
		return nil, err
	}
	filter := strings.TrimSpace(args.Filter)
	// A time bound keeps the search quick and to what was asked. A filter
	// that names a timestamp keeps its own.
	if !strings.Contains(filter, "timestamp") {
		bound := fmt.Sprintf("timestamp >= %q", since(args.SinceMinutes, time.Now()))
		if filter != "" {
			filter = "(" + filter + ") AND " + bound
		} else {
			filter = bound
		}
	}
	in := map[string]any{
		"resourceNames": []string{"projects/" + id},
		"filter":        filter,
		"orderBy":       "timestamp desc",
		"pageSize":      apitool.Limit(args.Limit, 25, 100),
	}
	var r struct {
		Entries []struct {
			Timestamp string `json:"timestamp"`
			Severity  string `json:"severity"`
			LogName   string `json:"logName"`
			Resource  struct {
				Type   string            `json:"type"`
				Labels map[string]string `json:"labels"`
			} `json:"resource"`
			TextPayload  string          `json:"textPayload"`
			JSONPayload  json.RawMessage `json:"jsonPayload"`
			ProtoPayload json.RawMessage `json:"protoPayload"`
		} `json:"entries"`
	}
	if err := a.call(ctx, conn, http.MethodPost, a.logging+"/v2/entries:list", in, &r); err != nil {
		return nil, err
	}
	out := []logEntry{}
	for _, e := range r.Entries {
		text := cmp.Or(e.TextPayload, string(e.JSONPayload), string(e.ProtoPayload))
		out = append(out, logEntry{
			Time: e.Timestamp, Severity: e.Severity, Log: logName(e.LogName),
			Resource: e.Resource.Type, Labels: e.Resource.Labels, Text: web.Cut(strings.TrimSpace(text), maxPayload, "…"),
		})
	}
	return out, nil
}

// logName is the log's own name, such as cloudaudit.googleapis.com/activity
// for projects/p/logs/cloudaudit.googleapis.com%2Factivity.
func logName(s string) string {
	_, name, ok := strings.Cut(s, "/logs/")
	if !ok {
		return s
	}
	if u, err := url.PathUnescape(name); err == nil {
		return u
	}
	return name
}

type series struct {
	Metric         string            `json:"metric"`
	MetricLabels   map[string]string `json:"metric_labels,omitempty"`
	Resource       string            `json:"resource"`
	ResourceLabels map[string]string `json:"resource_labels,omitempty"`
	Unit           string            `json:"unit,omitempty"`
	Points         []point           `json:"points"`
}

type point struct {
	Time  string `json:"time"`
	Value any    `json:"value"`
}

func (a *Adapter) metrics(ctx context.Context, conn channels.Connection, target string, raw json.RawMessage) (any, error) {
	args, err := apitool.Decode[struct {
		Filter                 string `json:"filter"`
		SinceMinutes           int    `json:"since_minutes"`
		AlignmentPeriodSeconds int    `json:"alignment_period_seconds"`
		Aligner                string `json:"aligner"`
		Limit                  int    `json:"limit"`
	}](raw)
	if err != nil {
		return nil, err
	}
	if err := apitool.Need("filter", args.Filter); err != nil {
		return nil, err
	}
	id, err := project(target)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	q := url.Values{
		"filter":             {args.Filter},
		"interval.startTime": {since(args.SinceMinutes, now)},
		"interval.endTime":   {now.UTC().Format(time.RFC3339)},
		"pageSize":           {strconv.Itoa(apitool.Limit(args.Limit, 25, 100))},
	}
	if args.AlignmentPeriodSeconds > 0 {
		aligner := cmp.Or(args.Aligner, "ALIGN_MEAN")
		if !validAligner.MatchString(aligner) {
			return nil, fmt.Errorf("%q is not an aligner, such as ALIGN_MEAN", web.Short(aligner, 40))
		}
		q.Set("aggregation.alignmentPeriod", strconv.Itoa(args.AlignmentPeriodSeconds)+"s")
		q.Set("aggregation.perSeriesAligner", aligner)
	}
	var r struct {
		TimeSeries []struct {
			Metric struct {
				Type   string            `json:"type"`
				Labels map[string]string `json:"labels"`
			} `json:"metric"`
			Resource struct {
				Type   string            `json:"type"`
				Labels map[string]string `json:"labels"`
			} `json:"resource"`
			Unit   string `json:"unit"`
			Points []struct {
				Interval struct {
					EndTime string `json:"endTime"`
				} `json:"interval"`
				Value typedValue `json:"value"`
			} `json:"points"`
		} `json:"timeSeries"`
	}
	if err := a.call(ctx, conn, http.MethodGet, a.monitoring+"/v3/projects/"+id+"/timeSeries?"+q.Encode(), nil, &r); err != nil {
		return nil, err
	}
	out := []series{}
	for _, ts := range r.TimeSeries {
		s := series{
			Metric: ts.Metric.Type, MetricLabels: ts.Metric.Labels,
			Resource: ts.Resource.Type, ResourceLabels: ts.Resource.Labels, Unit: ts.Unit, Points: []point{},
		}
		// Monitoring lists the newest point first.
		for _, p := range ts.Points[:min(len(ts.Points), maxPoints)] {
			s.Points = append(s.Points, point{Time: p.Interval.EndTime, Value: p.Value.value()})
		}
		out = append(out, s)
	}
	return out, nil
}

// typedValue is one point's value; Monitoring sets exactly one field.
type typedValue struct {
	DoubleValue       *float64 `json:"doubleValue"`
	Int64Value        *string  `json:"int64Value"`
	BoolValue         *bool    `json:"boolValue"`
	StringValue       *string  `json:"stringValue"`
	DistributionValue *struct {
		Count string  `json:"count"`
		Mean  float64 `json:"mean"`
	} `json:"distributionValue"`
}

func (v typedValue) value() any {
	switch {
	case v.DoubleValue != nil:
		return *v.DoubleValue
	case v.Int64Value != nil:
		// JSON carries int64 as a string, so it is not rounded.
		if n, err := strconv.ParseInt(*v.Int64Value, 10, 64); err == nil {
			return n
		}
		return *v.Int64Value
	case v.BoolValue != nil:
		return *v.BoolValue
	case v.StringValue != nil:
		return *v.StringValue
	case v.DistributionValue != nil:
		return map[string]any{"count": v.DistributionValue.Count, "mean": v.DistributionValue.Mean}
	}
	return nil
}

type bucket struct {
	Name         string `json:"name"`
	Location     string `json:"location"`
	StorageClass string `json:"storage_class,omitempty"`
	Created      string `json:"created,omitempty"`
}

func (a *Adapter) listBuckets(ctx context.Context, conn channels.Connection, target string, raw json.RawMessage) (any, error) {
	args, err := apitool.Decode[struct {
		Limit int `json:"limit"`
	}](raw)
	if err != nil {
		return nil, err
	}
	id, err := project(target)
	if err != nil {
		return nil, err
	}
	q := url.Values{"project": {id}, "maxResults": {strconv.Itoa(apitool.Limit(args.Limit, 25, 100))}}
	var r struct {
		Items []struct {
			Name         string `json:"name"`
			Location     string `json:"location"`
			StorageClass string `json:"storageClass"`
			TimeCreated  string `json:"timeCreated"`
		} `json:"items"`
	}
	if err := a.call(ctx, conn, http.MethodGet, a.storage+"/storage/v1/b?"+q.Encode(), nil, &r); err != nil {
		return nil, err
	}
	out := []bucket{}
	for _, b := range r.Items {
		out = append(out, bucket{Name: b.Name, Location: b.Location, StorageClass: b.StorageClass, Created: b.TimeCreated})
	}
	return out, nil
}

type object struct {
	Name        string `json:"name"`
	Size        int64  `json:"size"`
	ContentType string `json:"content_type,omitempty"`
	Updated     string `json:"updated,omitempty"`
}

type objectList struct {
	Bucket string `json:"bucket"`
	// Folders are the prefixes one level down, to list next.
	Folders []string `json:"folders,omitempty"`
	Objects []object `json:"objects"`
	More    bool     `json:"more,omitempty"`
}

func (a *Adapter) listObjects(ctx context.Context, conn channels.Connection, target string, raw json.RawMessage) (any, error) {
	args, err := apitool.Decode[struct {
		Bucket string `json:"bucket"`
		Prefix string `json:"prefix"`
		Limit  int    `json:"limit"`
	}](raw)
	if err != nil {
		return nil, err
	}
	id, err := project(target)
	if err != nil {
		return nil, err
	}
	if err := a.bucketIn(ctx, conn, id, args.Bucket); err != nil {
		return nil, err
	}
	q := url.Values{"delimiter": {"/"}, "maxResults": {strconv.Itoa(apitool.Limit(args.Limit, 25, 100))}}
	if args.Prefix != "" {
		q.Set("prefix", args.Prefix)
	}
	var r struct {
		Prefixes []string `json:"prefixes"`
		Items    []struct {
			Name        string `json:"name"`
			Size        string `json:"size"`
			ContentType string `json:"contentType"`
			Updated     string `json:"updated"`
		} `json:"items"`
		NextPageToken string `json:"nextPageToken"`
	}
	if err := a.call(ctx, conn, http.MethodGet, a.storage+"/storage/v1/b/"+url.PathEscape(args.Bucket)+"/o?"+q.Encode(), nil, &r); err != nil {
		return nil, err
	}
	out := objectList{Bucket: args.Bucket, Folders: r.Prefixes, Objects: []object{}, More: r.NextPageToken != ""}
	for _, o := range r.Items {
		size, _ := strconv.ParseInt(o.Size, 10, 64)
		out.Objects = append(out.Objects, object{Name: o.Name, Size: size, ContentType: o.ContentType, Updated: o.Updated})
	}
	return out, nil
}

type objectText struct {
	Bucket      string `json:"bucket"`
	Name        string `json:"name"`
	ContentType string `json:"content_type,omitempty"`
	web.Chunk
}

func (a *Adapter) readObject(ctx context.Context, conn channels.Connection, target string, raw json.RawMessage) (any, error) {
	args, err := apitool.Decode[struct {
		Bucket string `json:"bucket"`
		Name   string `json:"name"`
		Offset int64  `json:"offset"`
	}](raw)
	if err != nil {
		return nil, err
	}
	if err := apitool.Need("name", args.Name); err != nil {
		return nil, err
	}
	if args.Offset < 0 {
		return nil, errors.New("offset cannot be negative")
	}
	id, err := project(target)
	if err != nil {
		return nil, err
	}
	if err := a.bucketIn(ctx, conn, id, args.Bucket); err != nil {
		return nil, err
	}
	// PathEscape escapes the name's slashes too, as the JSON API wants.
	u := a.storage + "/storage/v1/b/" + url.PathEscape(args.Bucket) + "/o/" + url.PathEscape(args.Name)
	var meta struct {
		Size        string `json:"size"`
		ContentType string `json:"contentType"`
	}
	if err := a.call(ctx, conn, http.MethodGet, u, nil, &meta); err != nil {
		return nil, err
	}
	size, _ := strconv.ParseInt(meta.Size, 10, 64)
	if t := meta.ContentType; t != "" && !textual(t) {
		return nil, fmt.Errorf("%s is %s, not text, so it cannot be read here", args.Name, t)
	}
	if args.Offset > 0 && args.Offset >= size {
		return nil, fmt.Errorf("%s is %d bytes, shorter than the offset %d", args.Name, size, args.Offset)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u+"?alt=media", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Range", web.Range(args.Offset))
	b, err := a.send(conn, req, web.ChunkBytes)
	if err != nil {
		return nil, err
	}
	chunk, ok := web.TextChunk(b, args.Offset, max(size, args.Offset+int64(len(b))))
	if !ok {
		return nil, fmt.Errorf("%s is not text, so it cannot be read here", args.Name)
	}
	return objectText{Bucket: args.Bucket, Name: args.Name, ContentType: meta.ContentType, Chunk: chunk}, nil
}

// textual says whether a content type is text: text/*, and the structured
// formats served as application/*.
func textual(t string) bool {
	t, _, _ = strings.Cut(strings.ToLower(t), ";")
	t = strings.TrimSpace(t)
	if strings.HasPrefix(t, "text/") || strings.HasSuffix(t, "+json") || strings.HasSuffix(t, "+xml") {
		return true
	}
	switch t {
	case "application/json", "application/xml", "application/yaml", "application/x-yaml", "application/x-ndjson",
		"application/javascript", "application/sql", "application/toml", "application/csv", "application/octet-stream":
		// octet-stream is what an upload without a type gets; the bytes
		// decide.
		return true
	}
	return false
}

type operation struct {
	Instance  string `json:"instance"`
	Zone      string `json:"zone"`
	Done      string `json:"done"`
	Operation string `json:"operation,omitempty"`
	Status    string `json:"status,omitempty"`
}

func (a *Adapter) instanceAction(action string) func(context.Context, channels.Connection, string, json.RawMessage) (any, error) {
	return func(ctx context.Context, conn channels.Connection, target string, raw json.RawMessage) (any, error) {
		args, err := apitool.Decode[struct {
			Zone string `json:"zone"`
			Name string `json:"name"`
		}](raw)
		if err != nil {
			return nil, err
		}
		if !validZone.MatchString(args.Zone) {
			return nil, errors.New("give the VM's zone, such as europe-west6-a")
		}
		if !validInstance.MatchString(args.Name) {
			return nil, errors.New("give the VM's name, as list_instances shows it")
		}
		id, err := project(target)
		if err != nil {
			return nil, err
		}
		var op struct {
			Name   string `json:"name"`
			Status string `json:"status"`
		}
		u := a.compute + "/compute/v1/projects/" + id + "/zones/" + args.Zone + "/instances/" + args.Name + "/" + action
		if err := a.call(ctx, conn, http.MethodPost, u, nil, &op); err != nil {
			return nil, err
		}
		done := map[string]string{"start": "starting", "stop": "stopping"}[action]
		return operation{Instance: args.Name, Zone: args.Zone, Done: done, Operation: op.Name, Status: op.Status}, nil
	}
}
