package aws

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"math"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/bespinian/keera-agents/internal/channels/internal/apitool"
	"github.com/bespinian/keera-agents/internal/channels/internal/web"
)

const cloudwatchVersion = "2010-08-01"

type alarm struct {
	Name      string    `json:"name"`
	State     string    `json:"state"`
	Reason    string    `json:"reason,omitempty"`
	Namespace string    `json:"namespace,omitempty"`
	Metric    string    `json:"metric,omitempty"`
	Updated   time.Time `json:"updated"`
}

type alarmsArgs struct {
	State      string `json:"state"`
	NamePrefix string `json:"name_prefix"`
	Limit      int    `json:"limit"`
}

func alarms(ctx context.Context, c client, in alarmsArgs) (any, error) {
	p := url.Values{"Action": {"DescribeAlarms"}, "MaxRecords": {strconv.Itoa(apitool.Limit(in.Limit, 25, 100))}}
	if in.State != "" {
		p.Set("StateValue", in.State)
	}
	if in.NamePrefix != "" {
		p.Set("AlarmNamePrefix", in.NamePrefix)
	}
	var out struct {
		Alarms []struct {
			Name      string    `xml:"AlarmName"`
			State     string    `xml:"StateValue"`
			Reason    string    `xml:"StateReason"`
			Namespace string    `xml:"Namespace"`
			Metric    string    `xml:"MetricName"`
			Updated   time.Time `xml:"StateUpdatedTimestamp"`
		} `xml:"DescribeAlarmsResult>MetricAlarms>member"`
	}
	if err := c.query(ctx, "monitoring", cloudwatchVersion, p, &out); err != nil {
		return nil, err
	}
	res := []alarm{}
	for _, a := range out.Alarms {
		res = append(res, alarm{
			Name: a.Name, State: a.State, Reason: web.Cut(a.Reason, 300, "…"),
			Namespace: a.Namespace, Metric: a.Metric, Updated: a.Updated,
		})
	}
	return res, nil
}

// maxPoints is what CloudWatch returns for one request at most.
const maxPoints = 1440

type point struct {
	At    time.Time `json:"at"`
	Value float64   `json:"value"`
}

type series struct {
	Namespace string  `json:"namespace"`
	Metric    string  `json:"metric"`
	Statistic string  `json:"statistic"`
	Unit      string  `json:"unit,omitempty"`
	Period    int     `json:"period"`
	Points    []point `json:"points"`
}

type metricsArgs struct {
	Namespace  string            `json:"namespace"`
	MetricName string            `json:"metric_name"`
	Dimensions map[string]string `json:"dimensions"`
	Statistic  string            `json:"statistic"`
	Period     int               `json:"period"`
	Start      string            `json:"start"`
	End        string            `json:"end"`
}

func metrics(ctx context.Context, c client, in metricsArgs) (any, error) {
	if err := apitool.Need("namespace", in.Namespace); err != nil {
		return nil, err
	}
	if err := apitool.Need("metric_name", in.MetricName); err != nil {
		return nil, err
	}
	stat := in.Statistic
	if stat == "" {
		stat = "Average"
	}
	if !slices.Contains([]string{"Average", "Sum", "Minimum", "Maximum", "SampleCount"}, stat) {
		return nil, fmt.Errorf("%q is not a statistic; use Average, Sum, Minimum, Maximum or SampleCount", web.Short(stat, 40))
	}
	from, to, err := span(c, in.Start, in.End)
	if err != nil {
		return nil, err
	}
	secs := int(to.Sub(from).Seconds())
	period := in.Period
	if period <= 0 {
		// About 60 points, in whole minutes.
		period = max(60, (secs/60+59)/60*60)
	}
	if period%60 != 0 {
		return nil, fmt.Errorf("the period must be a multiple of 60 seconds, not %d", period)
	}
	if secs/period > maxPoints {
		return nil, fmt.Errorf("%d points of %d seconds is more than the %d CloudWatch returns; ask for a longer period or a shorter time",
			secs/period, period, maxPoints)
	}
	p := url.Values{
		"Action":              {"GetMetricStatistics"},
		"Namespace":           {in.Namespace},
		"MetricName":          {in.MetricName},
		"StartTime":           {from.UTC().Format(time.RFC3339)},
		"EndTime":             {to.UTC().Format(time.RFC3339)},
		"Period":              {strconv.Itoa(period)},
		"Statistics.member.1": {stat},
	}
	for i, name := range slices.Sorted(maps.Keys(in.Dimensions)) {
		n := "Dimensions.member." + strconv.Itoa(i+1)
		p.Set(n+".Name", name)
		p.Set(n+".Value", in.Dimensions[name])
	}
	var out struct {
		Points []struct {
			At          time.Time `xml:"Timestamp"`
			Unit        string    `xml:"Unit"`
			Average     float64   `xml:"Average"`
			Sum         float64   `xml:"Sum"`
			Minimum     float64   `xml:"Minimum"`
			Maximum     float64   `xml:"Maximum"`
			SampleCount float64   `xml:"SampleCount"`
		} `xml:"GetMetricStatisticsResult>Datapoints>member"`
	}
	if err := c.query(ctx, "monitoring", cloudwatchVersion, p, &out); err != nil {
		return nil, err
	}
	res := series{Namespace: in.Namespace, Metric: in.MetricName, Statistic: stat, Period: period, Points: []point{}}
	for _, d := range out.Points {
		v := map[string]float64{
			"Average": d.Average, "Sum": d.Sum, "Minimum": d.Minimum, "Maximum": d.Maximum, "SampleCount": d.SampleCount,
		}[stat]
		res.Points = append(res.Points, point{At: d.At, Value: v})
		res.Unit = d.Unit
	}
	// CloudWatch returns them in no particular order.
	slices.SortFunc(res.Points, func(a, b point) int { return a.At.Compare(b.At) })
	return res, nil
}

// validLogGroup is a log group's name as CloudWatch Logs allows it. It has
// no colon, so an ARN of a group in another region or account is refused.
var validLogGroup = regexp.MustCompile(`^[\w.\-/#]{1,512}$`)

// Events are trimmed to maxMessage bytes, and found in at most maxPages
// requests, since a filter that matches little may scan many empty pages.
const (
	maxMessage = 2000
	maxPages   = 5
)

type event struct {
	At      time.Time `json:"at"`
	Stream  string    `json:"stream"`
	Message string    `json:"message"`
}

type events struct {
	Events []event `json:"events"`
	// More says the search stopped before the end of the time asked for.
	More bool `json:"more,omitempty"`
}

type logsArgs struct {
	LogGroup      string `json:"log_group"`
	FilterPattern string `json:"filter_pattern"`
	Start         string `json:"start"`
	End           string `json:"end"`
	Limit         int    `json:"limit"`
}

func logsSearch(ctx context.Context, c client, in logsArgs) (any, error) {
	if !validLogGroup.MatchString(in.LogGroup) {
		return nil, fmt.Errorf("%q is not a log group's name, such as /aws/lambda/checkout", web.Short(in.LogGroup, 80))
	}
	from, to, err := span(c, in.Start, in.End)
	if err != nil {
		return nil, err
	}
	limit := apitool.Limit(in.Limit, 25, 100)
	res := events{Events: []event{}}
	next := ""
	for range maxPages {
		req := map[string]any{
			"logGroupName": in.LogGroup,
			"startTime":    from.UnixMilli(),
			"endTime":      to.UnixMilli(),
			"limit":        limit - len(res.Events),
		}
		if in.FilterPattern != "" {
			req["filterPattern"] = in.FilterPattern
		}
		if next != "" {
			req["nextToken"] = next
		}
		var out struct {
			Events []struct {
				Stream    string `json:"logStreamName"`
				Timestamp int64  `json:"timestamp"`
				Message   string `json:"message"`
			} `json:"events"`
			Next string `json:"nextToken"`
		}
		if err := c.callJSON(ctx, "logs", "Logs_20140328.FilterLogEvents", req, &out); err != nil {
			return nil, err
		}
		for _, e := range out.Events {
			res.Events = append(res.Events, event{
				At: time.UnixMilli(e.Timestamp).UTC(), Stream: e.Stream,
				Message: web.Cut(strings.TrimRight(e.Message, "\r\n"), maxMessage, "…"),
			})
		}
		next = out.Next
		if next == "" || len(res.Events) >= limit {
			break
		}
	}
	res.More = next != ""
	return res, nil
}

// maxGroups bounds the groups of one period; the rest are added up as Other.
const maxGroups = 15

type cost struct {
	Key    string  `json:"key"`
	Amount float64 `json:"amount"`
}

type costPeriod struct {
	Start     string  `json:"start"`
	End       string  `json:"end"`
	Amount    float64 `json:"amount"`
	Unit      string  `json:"unit"`
	Estimated bool    `json:"estimated,omitempty"`
	Groups    []cost  `json:"groups,omitempty"`
}

type costsArgs struct {
	Start       string `json:"start"`
	End         string `json:"end"`
	Granularity string `json:"granularity"`
	GroupBy     string `json:"group_by"`
}

type amount struct {
	Amount string `json:"Amount"`
	Unit   string `json:"Unit"`
}

func (a amount) value() float64 {
	v, _ := strconv.ParseFloat(a.Amount, 64)
	return math.Round(v*100) / 100
}

func costs(ctx context.Context, c client, in costsArgs) (any, error) {
	today := c.a.now().UTC().Truncate(24 * time.Hour)
	day := func(s string, def time.Time) (time.Time, error) {
		if s == "" {
			return def, nil
		}
		t, err := time.Parse(time.DateOnly, s)
		if err != nil {
			return t, fmt.Errorf("%q is not a day; write one such as 2026-01-31", web.Short(s, 40))
		}
		return t, nil
	}
	from, err := day(in.Start, today.AddDate(0, 0, 1-today.Day()))
	if err != nil {
		return nil, err
	}
	to, err := day(in.End, today.AddDate(0, 0, 1))
	if err != nil {
		return nil, err
	}
	if !from.Before(to) {
		return nil, fmt.Errorf("the start, %s, is not before the end, %s", from.Format(time.DateOnly), to.Format(time.DateOnly))
	}
	gran := in.Granularity
	if gran == "" {
		gran = "MONTHLY"
	}
	if gran != "MONTHLY" && gran != "DAILY" {
		return nil, fmt.Errorf("%q is not a granularity; use MONTHLY or DAILY", web.Short(gran, 40))
	}
	if gran == "DAILY" && to.Sub(from) > 62*24*time.Hour {
		return nil, fmt.Errorf("ask for at most 62 days by DAILY, or use MONTHLY")
	}
	req := map[string]any{
		"TimePeriod":  map[string]string{"Start": from.Format(time.DateOnly), "End": to.Format(time.DateOnly)},
		"Granularity": gran,
		"Metrics":     []string{"UnblendedCost"},
		// Costs are the account's, so they are narrowed to the binding's
		// region. Global services, such as Route 53, are left out.
		"Filter": map[string]any{"Dimensions": map[string]any{"Key": "REGION", "Values": []string{c.region}}},
	}
	switch in.GroupBy {
	case "":
	case "SERVICE", "USAGE_TYPE":
		req["GroupBy"] = []map[string]string{{"Type": "DIMENSION", "Key": in.GroupBy}}
	default:
		return nil, fmt.Errorf("%q cannot group costs; use SERVICE or USAGE_TYPE", web.Short(in.GroupBy, 40))
	}
	var out struct {
		Results []struct {
			Period struct {
				Start string `json:"Start"`
				End   string `json:"End"`
			} `json:"TimePeriod"`
			Total  map[string]amount `json:"Total"`
			Groups []struct {
				Keys    []string          `json:"Keys"`
				Metrics map[string]amount `json:"Metrics"`
			} `json:"Groups"`
			Estimated bool `json:"Estimated"`
		} `json:"ResultsByTime"`
	}
	// Cost Explorer has one endpoint, in us-east-1, whatever the region.
	ce := client{a: c.a, creds: c.creds, region: "us-east-1"}
	if err := ce.callJSON(ctx, "ce", "AWSInsightsIndexService.GetCostAndUsage", req, &out); err != nil {
		return nil, err
	}
	res := []costPeriod{}
	for _, r := range out.Results {
		p := costPeriod{Start: r.Period.Start, End: r.Period.End, Estimated: r.Estimated, Unit: "USD"}
		if t, ok := r.Total["UnblendedCost"]; ok {
			p.Amount, p.Unit = t.value(), t.Unit
		}
		var groups []cost
		total := 0.0
		for _, g := range r.Groups {
			m := g.Metrics["UnblendedCost"]
			groups = append(groups, cost{Key: strings.Join(g.Keys, ", "), Amount: m.value()})
			total += m.value()
			p.Unit = m.Unit
		}
		if len(r.Groups) > 0 {
			// Cost Explorer leaves the total empty when it groups.
			p.Amount = math.Round(total*100) / 100
		}
		slices.SortFunc(groups, func(a, b cost) int { return cmp.Compare(b.Amount, a.Amount) })
		if len(groups) > maxGroups {
			other := 0.0
			for _, g := range groups[maxGroups:] {
				other += g.Amount
			}
			groups = append(groups[:maxGroups], cost{Key: "Other", Amount: math.Round(other*100) / 100})
		}
		p.Groups = groups
		res = append(res, p)
	}
	return res, nil
}
