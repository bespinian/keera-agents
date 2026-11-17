package azure

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/bespinian/keera-agents/internal/connectors/internal/apitool"
	"github.com/bespinian/keera-agents/internal/connectors/internal/microsoft"
	"github.com/bespinian/keera-agents/internal/connectors/internal/web"
)

const (
	activityVersion = "2015-04-01"
	metricsVersion  = "2018-01-01"
	costVersion     = "2023-03-01"
	computeVersion  = "2024-07-01"

	// maxProperties bounds a resource's properties, in bytes of JSON, so a
	// large resource does not fill the model's context.
	maxProperties = 24 << 10
	// maxPoints bounds each metric's series.
	maxPoints = 500
	// maxPages bounds following ARM's nextLinks for one list.
	maxPages = 10
)

var (
	validGroup = regexp.MustCompile(`^[\p{L}\p{N}._()-]{1,90}$`)
	validType  = regexp.MustCompile(`^[A-Za-z0-9.]+/[A-Za-z0-9./]+$`)
	// validInterval is an ISO 8601 duration as Azure Monitor takes them,
	// such as PT5M, PT1H or P1D.
	validInterval = regexp.MustCompile(`^P(T[0-9]{1,4}[HMS]|[0-9]{1,3}D)$`)
)

func (a *Adapter) toolSet() apitool.Set {
	return apitool.Set{
		{
			Name:        "list_resource_groups",
			Description: "List the subscription's resource groups.",
			Parameters: apitool.Schema(`{"type":"object","properties":{
				"limit":{"type":"integer","description":"At most 100. Default 50."}}}`),
			Run: run(a.listGroups),
		},
		{
			Name:        "list_resources",
			Description: "List resources in the subscription or one resource group, with their ids, types, locations and tags.",
			Parameters: apitool.Schema(`{"type":"object","properties":{
				"resource_group":{"type":"string"},
				"type":{"type":"string","description":"A resource type, such as Microsoft.Compute/virtualMachines."},
				"limit":{"type":"integer","description":"At most 100. Default 25."}}}`),
			Run: run(a.listResources),
		},
		{
			Name:        "get_resource",
			Description: "Read one resource's settings and state by its id. A virtual machine includes its power state.",
			Parameters: apitool.Schema(`{"type":"object","required":["id"],"properties":{
				"id":{"type":"string","description":"The resource's id, starting /subscriptions/."}}}`),
			Run: run(a.getResource),
		},
		{
			Name:        "activity_log",
			Description: "List who changed what in the subscription, newest first. Default the last 24 hours.",
			Parameters: apitool.Schema(`{"type":"object","properties":{
				"start":{"type":"string","description":"RFC 3339 time or a date. At most 90 days ago."},
				"end":{"type":"string","description":"RFC 3339 time or a date. Default now."},
				"resource_group":{"type":"string"},
				"resource_id":{"type":"string","description":"Only this resource. Not with resource_group."},
				"limit":{"type":"integer","description":"At most 100. Default 50."}}}`),
			Run: run(a.activityLog),
		},
		{
			Name: "metrics",
			Description: "Read a resource's metrics from Azure Monitor, such as Percentage CPU of a virtual machine. " +
				"Without metric names, lists the ones the resource has.",
			Parameters: apitool.Schema(`{"type":"object","required":["resource_id"],"properties":{
				"resource_id":{"type":"string"},
				"metrics":{"type":"array","items":{"type":"string"},"description":"Metric names, such as Percentage CPU."},
				"start":{"type":"string","description":"RFC 3339 time or a date. Default 24 hours ago."},
				"end":{"type":"string","description":"Default now."},
				"interval":{"type":"string","description":"ISO 8601, such as PT5M, PT1H or P1D. Default PT1H."},
				"aggregation":{"type":"string","enum":["Average","Total","Minimum","Maximum","Count"],"description":"Default Average."}}}`),
			Run: run(a.metrics),
		},
		{
			Name:        "costs",
			Description: "Read the subscription's actual costs for a date range, in total or by resource group or service.",
			Parameters: apitool.Schema(`{"type":"object","properties":{
				"start":{"type":"string","description":"A date, such as 2026-11-01. Default the first of this month."},
				"end":{"type":"string","description":"A date, included. Default today."},
				"group_by":{"type":"string","enum":["resource_group","service"]},
				"daily":{"type":"boolean","description":"A row per day, not one for the whole range."}}}`),
			Run: run(a.costs),
		},
		vmTool("vm_start", "Start a virtual machine.", "start", a),
		vmTool("vm_stop", "Stop a virtual machine and release its compute, so it stops costing. Its disks are kept.", "deallocate", a),
		vmTool("vm_restart", "Restart a running virtual machine.", "restart", a),
	}
}

func vmTool(name, desc, action string, a *Adapter) apitool.Tool {
	return apitool.Tool{
		Name:        name,
		Description: desc + " Azure does it in the background; check with get_resource.",
		Parameters: apitool.Schema(`{"type":"object","required":["id"],"properties":{
			"id":{"type":"string","description":"The virtual machine's resource id."}}}`),
		Write: true,
		Run: run(func(ctx context.Context, app microsoft.App, sub string, in vmArgs) (any, error) {
			return a.vmAction(ctx, app, sub, in, action)
		}),
	}
}

// Resource is one Azure resource. Properties is set only when it is read
// on its own.
type Resource struct {
	ID         string            `json:"id"`
	Name       string            `json:"name"`
	Type       string            `json:"type,omitempty"`
	Location   string            `json:"location,omitempty"`
	Kind       string            `json:"kind,omitempty"`
	Tags       map[string]string `json:"tags,omitempty"`
	SKU        json.RawMessage   `json:"sku,omitempty"`
	Properties any               `json:"properties,omitempty"`
}

type resource struct {
	ID         string            `json:"id"`
	Name       string            `json:"name"`
	Type       string            `json:"type"`
	Location   string            `json:"location"`
	Kind       string            `json:"kind"`
	Tags       map[string]string `json:"tags"`
	SKU        json.RawMessage   `json:"sku"`
	Properties json.RawMessage   `json:"properties"`
}

func (r resource) brief() Resource {
	return Resource{ID: r.ID, Name: r.Name, Type: r.Type, Location: r.Location, Kind: r.Kind, Tags: r.Tags}
}

// list follows ARM's pages from u until it has limit resources.
func (a *Adapter) list(ctx context.Context, app microsoft.App, u string, limit int) ([]resource, error) {
	var out []resource
	for range maxPages {
		var r struct {
			Value    []resource `json:"value"`
			NextLink string     `json:"nextLink"`
		}
		if err := a.call(ctx, app, http.MethodGet, u, nil, &r); err != nil {
			return nil, err
		}
		out = append(out, r.Value...)
		if len(out) >= limit || r.NextLink == "" {
			break
		}
		u = r.NextLink
	}
	return out[:min(len(out), limit)], nil
}

func group(s string) (string, error) {
	if !validGroup.MatchString(s) {
		return "", fmt.Errorf("%q is not a resource group's name", web.Short(s, 80))
	}
	return url.PathEscape(s), nil
}

func (a *Adapter) listGroups(ctx context.Context, app microsoft.App, sub string, in struct {
	Limit int `json:"limit"`
},
) (any, error) {
	limit := apitool.Limit(in.Limit, 50, 100)
	q := map[string]string{"api-version": resourcesVersion, "$top": strconv.Itoa(limit)}
	list, err := a.list(ctx, app, a.arm+"/subscriptions/"+sub+"/resourcegroups"+microsoft.Query(q), limit)
	if err != nil {
		return nil, err
	}
	out := make([]Resource, len(list))
	for i, r := range list {
		out[i] = Resource{ID: r.ID, Name: r.Name, Location: r.Location, Tags: r.Tags}
	}
	return out, nil
}

func (a *Adapter) listResources(ctx context.Context, app microsoft.App, sub string, in struct {
	ResourceGroup string `json:"resource_group"`
	Type          string `json:"type"`
	Limit         int    `json:"limit"`
},
) (any, error) {
	limit := apitool.Limit(in.Limit, 25, 100)
	path := "/subscriptions/" + sub
	if in.ResourceGroup != "" {
		g, err := group(in.ResourceGroup)
		if err != nil {
			return nil, err
		}
		path += "/resourceGroups/" + g
	}
	q := map[string]string{"api-version": resourcesVersion, "$top": strconv.Itoa(limit)}
	if in.Type != "" {
		if !validType.MatchString(in.Type) {
			return nil, fmt.Errorf("%q is not a resource type, such as Microsoft.Compute/virtualMachines", web.Short(in.Type, 80))
		}
		q["$filter"] = "resourceType eq '" + in.Type + "'"
	}
	list, err := a.list(ctx, app, a.arm+path+"/resources"+microsoft.Query(q), limit)
	if err != nil {
		return nil, err
	}
	out := make([]Resource, len(list))
	for i, r := range list {
		out[i] = r.brief()
	}
	return out, nil
}

func (a *Adapter) getResource(ctx context.Context, app microsoft.App, sub string, in struct {
	ID string `json:"id"`
},
) (any, error) {
	id, err := parseID(sub, in.ID)
	if err != nil {
		return nil, err
	}
	v, err := a.apiVersion(ctx, app, sub, id)
	if err != nil {
		return nil, err
	}
	q := map[string]string{"api-version": v}
	if id.isVM() {
		// The power state lives in the instance view.
		q["$expand"] = "instanceView"
	}
	var r resource
	if err := a.call(ctx, app, http.MethodGet, a.arm+id.path+microsoft.Query(q), nil, &r); err != nil {
		return nil, err
	}
	out := r.brief()
	if out.ID == "" {
		out.ID = id.id
	}
	out.SKU = r.SKU
	if len(r.Properties) > maxProperties {
		out.Properties = web.Cut(string(r.Properties), maxProperties, " [trimmed]")
	} else if len(r.Properties) > 0 {
		out.Properties = r.Properties
	}
	return out, nil
}

// parseTime reads an RFC 3339 time or a date, in UTC; empty is def.
func parseTime(s string, def time.Time) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return def, nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC(), nil
	}
	if t, err := time.Parse(time.DateOnly, s); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("%q is not a time; write 2026-11-04T08:00:00Z or 2026-11-04", web.Short(s, 40))
}

// timeRange reads start and end, by default the span before now, and
// checks they make a range of at most max.
func timeRange(start, end string, span, max time.Duration) (time.Time, time.Time, error) {
	now := time.Now().UTC().Truncate(time.Second)
	to, err := parseTime(end, now)
	if err != nil {
		return to, to, err
	}
	from, err := parseTime(start, to.Add(-span))
	if err != nil {
		return from, to, err
	}
	switch {
	case !to.After(from):
		return from, to, errors.New("end must be after start")
	case to.Sub(from) > max:
		return from, to, fmt.Errorf("ask for at most %d days at a time", int(max.Hours()/24))
	}
	return from, to, nil
}

// Event is one entry of the activity log.
type Event struct {
	Time       time.Time `json:"time"`
	Operation  string    `json:"operation"`
	Status     string    `json:"status,omitempty"`
	Caller     string    `json:"caller,omitempty"`
	Level      string    `json:"level,omitempty"`
	ResourceID string    `json:"resource_id,omitempty"`
}

type localized struct {
	Value          string `json:"value"`
	LocalizedValue string `json:"localizedValue"`
}

func (l localized) String() string {
	if l.LocalizedValue != "" {
		return l.LocalizedValue
	}
	return l.Value
}

func (a *Adapter) activityLog(ctx context.Context, app microsoft.App, sub string, in struct {
	Start         string `json:"start"`
	End           string `json:"end"`
	ResourceGroup string `json:"resource_group"`
	ResourceID    string `json:"resource_id"`
	Limit         int    `json:"limit"`
},
) (any, error) {
	from, to, err := timeRange(in.Start, in.End, 24*time.Hour, 90*24*time.Hour)
	if err != nil {
		return nil, err
	}
	filter := "eventTimestamp ge '" + from.Format(time.RFC3339) + "' and eventTimestamp le '" + to.Format(time.RFC3339) + "'"
	switch {
	case in.ResourceGroup != "" && in.ResourceID != "":
		return nil, errors.New("give resource_group or resource_id, not both")
	case in.ResourceGroup != "":
		if _, err := group(in.ResourceGroup); err != nil {
			return nil, err
		}
		filter += " and resourceGroupName eq '" + in.ResourceGroup + "'"
	case in.ResourceID != "":
		id, err := parseID(sub, in.ResourceID)
		if err != nil {
			return nil, err
		}
		filter += " and resourceUri eq '" + id.id + "'"
	}
	limit := apitool.Limit(in.Limit, 50, 100)
	q := map[string]string{
		"api-version": activityVersion,
		"$filter":     filter,
		"$select":     "eventTimestamp,operationName,status,caller,level,resourceId",
	}
	u := a.arm + "/subscriptions/" + sub + "/providers/Microsoft.Insights/eventtypes/management/values" + microsoft.Query(q)
	out := []Event{}
	// The log may answer with empty pages before the full ones.
	for range maxPages {
		var r struct {
			Value []struct {
				EventTimestamp time.Time `json:"eventTimestamp"`
				OperationName  localized `json:"operationName"`
				Status         localized `json:"status"`
				Caller         string    `json:"caller"`
				Level          string    `json:"level"`
				ResourceID     string    `json:"resourceId"`
			} `json:"value"`
			NextLink string `json:"nextLink"`
		}
		if err := a.call(ctx, app, http.MethodGet, u, nil, &r); err != nil {
			return nil, err
		}
		for _, e := range r.Value {
			out = append(out, Event{
				Time: e.EventTimestamp, Operation: e.OperationName.String(), Status: e.Status.String(),
				Caller: e.Caller, Level: e.Level, ResourceID: e.ResourceID,
			})
		}
		if len(out) >= limit || r.NextLink == "" {
			break
		}
		u = r.NextLink
	}
	return out[:min(len(out), limit)], nil
}

// Series is one metric over time, in its chosen aggregation.
type Series struct {
	Metric      string  `json:"metric"`
	Unit        string  `json:"unit,omitempty"`
	Aggregation string  `json:"aggregation"`
	Points      []Point `json:"points"`
}

type Point struct {
	Time  time.Time `json:"time"`
	Value float64   `json:"value"`
}

var aggregations = []string{"Average", "Total", "Minimum", "Maximum", "Count"}

func (a *Adapter) metrics(ctx context.Context, app microsoft.App, sub string, in struct {
	ResourceID  string   `json:"resource_id"`
	Metrics     []string `json:"metrics"`
	Start       string   `json:"start"`
	End         string   `json:"end"`
	Interval    string   `json:"interval"`
	Aggregation string   `json:"aggregation"`
},
) (any, error) {
	id, err := parseID(sub, in.ResourceID)
	if err != nil {
		return nil, err
	}
	if len(in.Metrics) == 0 {
		return a.metricNames(ctx, app, id)
	}
	if len(in.Metrics) > 20 {
		return nil, errors.New("ask for at most 20 metrics at a time")
	}
	for _, m := range in.Metrics {
		// Azure separates the names with commas.
		if strings.TrimSpace(m) == "" || strings.Contains(m, ",") {
			return nil, fmt.Errorf("%q is not a metric name", web.Short(m, 80))
		}
	}
	from, to, err := timeRange(in.Start, in.End, 24*time.Hour, 93*24*time.Hour)
	if err != nil {
		return nil, err
	}
	interval := strings.ToUpper(strings.TrimSpace(in.Interval))
	if interval == "" {
		interval = "PT1H"
	}
	if !validInterval.MatchString(interval) {
		return nil, fmt.Errorf("%q is not an interval; write PT5M, PT1H or P1D", web.Short(in.Interval, 40))
	}
	agg := "Average"
	if in.Aggregation != "" {
		agg = ""
		for _, x := range aggregations {
			if strings.EqualFold(x, in.Aggregation) {
				agg = x
			}
		}
		if agg == "" {
			return nil, fmt.Errorf("the aggregation is one of %s", strings.Join(aggregations, ", "))
		}
	}
	q := map[string]string{
		"api-version": metricsVersion,
		"metricnames": strings.Join(in.Metrics, ","),
		"timespan":    from.Format(time.RFC3339) + "/" + to.Format(time.RFC3339),
		"interval":    interval,
		"aggregation": agg,
	}
	var r struct {
		Value []struct {
			Name       localized `json:"name"`
			Unit       string    `json:"unit"`
			Timeseries []struct {
				Data []map[string]any `json:"data"`
			} `json:"timeseries"`
		} `json:"value"`
	}
	if err := a.call(ctx, app, http.MethodGet, a.arm+id.path+"/providers/Microsoft.Insights/metrics"+microsoft.Query(q), nil, &r); err != nil {
		return nil, err
	}
	key := strings.ToLower(agg)
	out := []Series{}
	for _, m := range r.Value {
		s := Series{Metric: m.Name.String(), Unit: m.Unit, Aggregation: agg, Points: []Point{}}
		for _, ts := range m.Timeseries {
			for _, d := range ts.Data {
				// Intervals without data have no value at all.
				v, ok := d[key].(float64)
				at, _ := d["timeStamp"].(string)
				t, err := time.Parse(time.RFC3339, at)
				if ok && err == nil && len(s.Points) < maxPoints {
					s.Points = append(s.Points, Point{Time: t, Value: v})
				}
			}
		}
		out = append(out, s)
	}
	return out, nil
}

// metricNames lists the metrics a resource has, for a model that does not
// know their names.
func (a *Adapter) metricNames(ctx context.Context, app microsoft.App, id resourceID) (any, error) {
	var r struct {
		Value []struct {
			Name                   localized `json:"name"`
			Unit                   string    `json:"unit"`
			PrimaryAggregationType string    `json:"primaryAggregationType"`
		} `json:"value"`
	}
	q := map[string]string{"api-version": metricsVersion}
	if err := a.call(ctx, app, http.MethodGet, a.arm+id.path+"/providers/Microsoft.Insights/metricDefinitions"+microsoft.Query(q), nil, &r); err != nil {
		return nil, err
	}
	type def struct {
		Name        string `json:"name"`
		Unit        string `json:"unit,omitempty"`
		Aggregation string `json:"aggregation,omitempty"`
	}
	defs := make([]def, len(r.Value))
	for i, m := range r.Value {
		defs[i] = def{Name: m.Name.Value, Unit: m.Unit, Aggregation: m.PrimaryAggregationType}
	}
	return map[string]any{"note": "No metrics were named; these are the ones the resource has.", "metrics": defs}, nil
}

// Costs is what the subscription cost in a date range.
type Costs struct {
	Start    string    `json:"start"`
	End      string    `json:"end"`
	Currency string    `json:"currency,omitempty"`
	Total    float64   `json:"total"`
	Rows     []CostRow `json:"rows,omitempty"`
	// More is how many rows were left out, the cheapest.
	More int `json:"more,omitempty"`
}

type CostRow struct {
	Date  string  `json:"date,omitempty"`
	Group string  `json:"group,omitempty"`
	Cost  float64 `json:"cost"`
}

func (a *Adapter) costs(ctx context.Context, app microsoft.App, sub string, in struct {
	Start   string `json:"start"`
	End     string `json:"end"`
	GroupBy string `json:"group_by"`
	Daily   bool   `json:"daily"`
},
) (any, error) {
	today := time.Now().UTC().Truncate(24 * time.Hour)
	to, err := parseTime(in.End, today)
	if err != nil {
		return nil, err
	}
	from, err := parseTime(in.Start, time.Date(to.Year(), to.Month(), 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		return nil, err
	}
	from, to = from.Truncate(24*time.Hour), to.Truncate(24*time.Hour)
	switch {
	case to.Before(from):
		return nil, errors.New("end must not be before start")
	case to.Sub(from) > 366*24*time.Hour:
		return nil, errors.New("ask for at most a year at a time")
	}
	dataset := map[string]any{
		"granularity": "None",
		"aggregation": map[string]any{"totalCost": map[string]string{"name": "Cost", "function": "Sum"}},
	}
	if in.Daily {
		dataset["granularity"] = "Daily"
	}
	switch in.GroupBy {
	case "":
	case "resource_group":
		dataset["grouping"] = []map[string]string{{"type": "Dimension", "name": "ResourceGroupName"}}
	case "service":
		dataset["grouping"] = []map[string]string{{"type": "Dimension", "name": "ServiceName"}}
	default:
		return nil, errors.New("group_by is resource_group or service")
	}
	body := map[string]any{
		"type":       "ActualCost",
		"timeframe":  "Custom",
		"timePeriod": map[string]string{"from": from.Format(time.RFC3339), "to": to.Add(24*time.Hour - time.Second).Format(time.RFC3339)},
		"dataset":    dataset,
	}
	var r struct {
		Properties struct {
			Columns []struct {
				Name string `json:"name"`
				Type string `json:"type"`
			} `json:"columns"`
			Rows [][]any `json:"rows"`
		} `json:"properties"`
	}
	u := a.arm + "/subscriptions/" + sub + "/providers/Microsoft.CostManagement/query" + microsoft.Query(map[string]string{"api-version": costVersion})
	err = a.call(ctx, app, http.MethodPost, u, body, &r)
	if web.Status(err) == http.StatusForbidden {
		return nil, fmt.Errorf("%s may not read costs; assign Cost Management Reader on the subscription (%w)", app.Who, err)
	}
	if err != nil {
		return nil, err
	}
	cost, date, grp, cur := -1, -1, -1, -1
	for i, c := range r.Properties.Columns {
		switch {
		case c.Name == "UsageDate":
			date = i
		case c.Name == "Currency":
			cur = i
		case c.Name == "ResourceGroupName" || c.Name == "ServiceName":
			grp = i
		case cost < 0 && c.Type == "Number":
			cost = i
		}
	}
	out := Costs{Start: from.Format(time.DateOnly), End: to.Format(time.DateOnly), Rows: []CostRow{}}
	if cost < 0 {
		return out, nil
	}
	for _, row := range r.Properties.Rows {
		var c CostRow
		c.Cost, _ = cell(row, cost).(float64)
		if d, ok := cell(row, date).(float64); ok {
			// Usage dates come as numbers, such as 20261104.
			if t, err := time.Parse("20060102", strconv.FormatInt(int64(d), 10)); err == nil {
				c.Date = t.Format(time.DateOnly)
			}
		}
		c.Group, _ = cell(row, grp).(string)
		if s, ok := cell(row, cur).(string); ok && s != "" {
			out.Currency = s
		}
		out.Total += c.Cost
		c.Cost = round(c.Cost)
		if in.GroupBy != "" || in.Daily {
			out.Rows = append(out.Rows, c)
		}
	}
	out.Total = round(out.Total)
	sort.SliceStable(out.Rows, func(i, j int) bool {
		if out.Rows[i].Date != out.Rows[j].Date {
			return out.Rows[i].Date < out.Rows[j].Date
		}
		return out.Rows[i].Cost > out.Rows[j].Cost
	})
	if len(out.Rows) > 100 {
		out.More = len(out.Rows) - 100
		out.Rows = out.Rows[:100]
	}
	return out, nil
}

func cell(row []any, i int) any {
	if i < 0 || i >= len(row) {
		return nil
	}
	return row[i]
}

func round(x float64) float64 {
	return math.Round(x*100) / 100
}

type vmArgs struct {
	ID string `json:"id"`
}

func (a *Adapter) vmAction(ctx context.Context, app microsoft.App, sub string, in vmArgs, action string) (any, error) {
	id, err := parseID(sub, in.ID)
	if err != nil {
		return nil, err
	}
	if !id.isVM() || len(id.segs) != 8 {
		return nil, fmt.Errorf("%q is not a virtual machine's id", web.Short(in.ID, 120))
	}
	u := a.arm + id.path + "/" + action + microsoft.Query(map[string]string{"api-version": computeVersion})
	err = a.call(ctx, app, http.MethodPost, u, nil, nil)
	if web.Status(err) == http.StatusForbidden {
		return nil, fmt.Errorf("%s may not %s %s; assign Virtual Machine Contributor (%w)", app.Who, action, id.name(), err)
	}
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"accepted": true, "vm": id.name(), "action": action,
		"note": "Azure is doing it now; it takes a minute or two.",
	}, nil
}
