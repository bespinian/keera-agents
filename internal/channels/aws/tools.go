package aws

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/bespinian/keera-agents/internal/channels"
	"github.com/bespinian/keera-agents/internal/channels/internal/apitool"
	"github.com/bespinian/keera-agents/internal/channels/internal/web"
)

func (a *Adapter) toolSet() apitool.Set {
	return apitool.Set{
		tool(a, "whoami", "Who the agent is in AWS: the account and the IAM ARN it acts as.", false,
			`{"type":"object","properties":{}}`,
			func(ctx context.Context, c client, _ struct{}) (any, error) { return c.identity(ctx) }),
		tool(a, "ec2_instances", "List EC2 instances in the region: id, Name tag, state, type, zone, IP addresses and launch time.", false,
			`{"type":"object","properties":{
				"instance_ids":{"type":"array","items":{"type":"string"},"description":"Only these instances, such as i-0abcd1234ef567890."},
				"filters":{"type":"object","additionalProperties":{"type":"array","items":{"type":"string"}},
					"description":"EC2 filters, name to values, such as {\"tag:Name\":[\"web-*\"],\"instance-state-name\":[\"running\"]}."},
				"limit":{"type":"integer","description":"At most this many instances: 25 unless given, up to 100."}}}`,
			ec2Instances),
		tool(a, "ec2_start", "Start stopped EC2 instances.", true, idsSchema, ec2Change("StartInstances")),
		tool(a, "ec2_stop", "Stop running EC2 instances. Their local instance storage is lost.", true, idsSchema, ec2Change("StopInstances")),
		tool(a, "ec2_reboot", "Reboot running EC2 instances.", true, idsSchema, ec2Change("RebootInstances")),
		tool(a, "s3_buckets", "List the S3 buckets in the region.", false,
			`{"type":"object","properties":{
				"limit":{"type":"integer","description":"At most this many buckets: 25 unless given, up to 100."}}}`,
			s3Buckets),
		tool(a, "s3_list", "List objects in an S3 bucket, one folder at a time unless recursive.", false,
			`{"type":"object","properties":{
				"bucket":{"type":"string"},
				"prefix":{"type":"string","description":"Only keys starting with this, such as reports/2026/."},
				"recursive":{"type":"boolean","description":"List every key under the prefix instead of one folder."},
				"limit":{"type":"integer","description":"At most this many objects: 25 unless given, up to 100."},
				"after":{"type":"string","description":"The next value of the previous list, for the next page."}},
				"required":["bucket"]}`,
			s3List),
		tool(a, "s3_read", "Read a text object from S3, 48 KiB at a time. For the rest, read again from next_offset.", false,
			`{"type":"object","properties":{"bucket":{"type":"string"},"key":{"type":"string"},
				"offset":{"type":"integer","description":"The byte to start from: 0, or an earlier read's next_offset."}},
				"required":["bucket","key"]}`,
			s3Read),
		tool(a, "s3_write", "Write a text object to S3, replacing any object with the same key.", true,
			`{"type":"object","properties":{
				"bucket":{"type":"string"},
				"key":{"type":"string","description":"Such as reports/2026-11.md."},
				"content":{"type":"string"},
				"content_type":{"type":"string","description":"Such as text/csv. text/plain unless given."}},
				"required":["bucket","key","content"]}`,
			s3Write),
		tool(a, "cloudwatch_alarms", "List CloudWatch metric alarms: name, state, reason, metric and when the state changed.", false,
			`{"type":"object","properties":{
				"state":{"type":"string","enum":["OK","ALARM","INSUFFICIENT_DATA"],"description":"Only alarms in this state."},
				"name_prefix":{"type":"string","description":"Only alarms whose name starts with this."},
				"limit":{"type":"integer","description":"At most this many alarms: 25 unless given, up to 100."}}}`,
			alarms),
		tool(a, "cloudwatch_metrics", "Read one CloudWatch metric's statistic over time, such as AWS/EC2 CPUUtilization of one instance.", false,
			`{"type":"object","properties":{
				"namespace":{"type":"string","description":"Such as AWS/EC2."},
				"metric_name":{"type":"string","description":"Such as CPUUtilization."},
				"dimensions":{"type":"object","additionalProperties":{"type":"string"},"description":"Such as {\"InstanceId\":\"i-0abcd1234ef567890\"}."},
				"statistic":{"type":"string","enum":["Average","Sum","Minimum","Maximum","SampleCount"],"description":"Average unless given."},
				"period":{"type":"integer","description":"Seconds per point, a multiple of 60. Chosen for about 60 points unless given."},
				"start":{"type":"string","description":"RFC 3339 time, or how long ago, such as 6h or 7d. An hour ago unless given."},
				"end":{"type":"string","description":"Like start. Now unless given."}},
				"required":["namespace","metric_name"]}`,
			metrics),
		tool(a, "logs_search", "Search a CloudWatch Logs log group's events, newest last.", false,
			`{"type":"object","properties":{
				"log_group":{"type":"string","description":"Such as /aws/lambda/checkout."},
				"filter_pattern":{"type":"string","description":"CloudWatch Logs filter pattern, such as ERROR or \"timed out\". Every event unless given."},
				"start":{"type":"string","description":"RFC 3339 time, or how long ago, such as 6h or 7d. An hour ago unless given."},
				"end":{"type":"string","description":"Like start. Now unless given."},
				"limit":{"type":"integer","description":"At most this many events: 25 unless given, up to 100."}},
				"required":["log_group"]}`,
			logsSearch),
		tool(a, "costs", "What the region cost, unblended, from Cost Explorer. Each call costs USD 0.01.", false,
			`{"type":"object","properties":{
				"start":{"type":"string","description":"First day, YYYY-MM-DD. The first of this month unless given."},
				"end":{"type":"string","description":"Day after the last, YYYY-MM-DD. Tomorrow unless given."},
				"granularity":{"type":"string","enum":["MONTHLY","DAILY"],"description":"MONTHLY unless given."},
				"group_by":{"type":"string","enum":["SERVICE","USAGE_TYPE"],"description":"Split each period's cost by this."}}}`,
			costs),
	}
}

// tool makes a tool whose run gets its arguments decoded and a client for
// the binding's region.
func tool[T any](a *Adapter, name, desc string, write bool, schema string, run func(context.Context, client, T) (any, error)) apitool.Tool {
	return apitool.Tool{
		Name:        name,
		Description: desc,
		Parameters:  apitool.Schema(schema),
		Write:       write,
		Run: func(ctx context.Context, conn channels.Connection, target string, args json.RawMessage) (any, error) {
			in, err := apitool.Decode[T](args)
			if err != nil {
				return nil, err
			}
			c, err := a.client(ctx, conn, target)
			if err != nil {
				return nil, err
			}
			return run(ctx, c, in)
		},
	}
}

// when reads a time as RFC 3339, a date, or how long ago, such as 6h or 7d.
func when(s string, now, def time.Time) (time.Time, error) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "-")
	if s == "" {
		return def, nil
	}
	if d, err := time.ParseDuration(s); err == nil {
		return now.Add(-d), nil
	}
	if days, ok := strings.CutSuffix(s, "d"); ok {
		if n, err := strconv.Atoi(days); err == nil {
			return now.AddDate(0, 0, -n), nil
		}
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04", "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("%q is not a time; write one such as 2026-01-31T14:00:00Z, or how long ago, such as 6h", web.Short(s, 40))
}

// span reads a start and end, an hour ago and now unless given.
func span(c client, start, end string) (time.Time, time.Time, error) {
	now := c.a.now().UTC()
	to, err := when(end, now, now)
	if err != nil {
		return to, to, err
	}
	from, err := when(start, now, to.Add(-time.Hour))
	if err == nil && !from.Before(to) {
		err = fmt.Errorf("the start, %s, is not before the end, %s", from.Format(time.RFC3339), to.Format(time.RFC3339))
	}
	return from, to, err
}

const ec2Version = "2016-11-15"

var validInstance = regexp.MustCompile(`^i-[0-9a-f]{8,17}$`)

const idsSchema = `{"type":"object","properties":{
	"instance_ids":{"type":"array","items":{"type":"string"},"description":"Such as [\"i-0abcd1234ef567890\"]."}},
	"required":["instance_ids"]}`

// addIDs checks instance ids and adds them as InstanceId.N.
func addIDs(p url.Values, ids []string) error {
	if len(ids) > 100 {
		return fmt.Errorf("give at most 100 instance ids, not %d", len(ids))
	}
	for i, id := range ids {
		if !validInstance.MatchString(id) {
			return fmt.Errorf("%q is not an instance id, such as i-0abcd1234ef567890", web.Short(id, 40))
		}
		p.Set("InstanceId."+strconv.Itoa(i+1), id)
	}
	return nil
}

type instance struct {
	ID        string    `json:"id"`
	Name      string    `json:"name,omitempty"`
	State     string    `json:"state"`
	Type      string    `json:"type"`
	Zone      string    `json:"zone"`
	PrivateIP string    `json:"private_ip,omitempty"`
	PublicIP  string    `json:"public_ip,omitempty"`
	Launched  time.Time `json:"launched"`
}

type instances struct {
	Instances []instance `json:"instances"`
	// More says there were more than the limit, so the agent narrows the
	// filters rather than missing some silently.
	More bool `json:"more,omitempty"`
}

type instancesArgs struct {
	InstanceIDs []string            `json:"instance_ids"`
	Filters     map[string][]string `json:"filters"`
	Limit       int                 `json:"limit"`
}

func ec2Instances(ctx context.Context, c client, in instancesArgs) (any, error) {
	limit := apitool.Limit(in.Limit, 25, 100)
	p := url.Values{"Action": {"DescribeInstances"}}
	if err := addIDs(p, in.InstanceIDs); err != nil {
		return nil, err
	}
	if len(in.InstanceIDs) == 0 {
		// EC2 refuses MaxResults with instance ids, and below 5.
		p.Set("MaxResults", strconv.Itoa(max(limit, 5)))
	}
	for i, name := range slices.Sorted(maps.Keys(in.Filters)) {
		n := "Filter." + strconv.Itoa(i+1)
		p.Set(n+".Name", name)
		for j, v := range in.Filters[name] {
			p.Set(n+".Value."+strconv.Itoa(j+1), v)
		}
	}
	var out struct {
		Reservations []struct {
			Instances []struct {
				ID        string    `xml:"instanceId"`
				State     string    `xml:"instanceState>name"`
				Type      string    `xml:"instanceType"`
				Zone      string    `xml:"placement>availabilityZone"`
				PrivateIP string    `xml:"privateIpAddress"`
				PublicIP  string    `xml:"ipAddress"`
				Launched  time.Time `xml:"launchTime"`
				Tags      []struct {
					Key   string `xml:"key"`
					Value string `xml:"value"`
				} `xml:"tagSet>item"`
			} `xml:"instancesSet>item"`
		} `xml:"reservationSet>item"`
		Next string `xml:"nextToken"`
	}
	if err := c.query(ctx, "ec2", ec2Version, p, &out); err != nil {
		return nil, err
	}
	res := instances{Instances: []instance{}, More: out.Next != ""}
	for _, r := range out.Reservations {
		for _, i := range r.Instances {
			if len(res.Instances) == limit {
				res.More = true
				return res, nil
			}
			v := instance{
				ID: i.ID, State: i.State, Type: i.Type, Zone: i.Zone,
				PrivateIP: i.PrivateIP, PublicIP: i.PublicIP, Launched: i.Launched,
			}
			for _, t := range i.Tags {
				if t.Key == "Name" {
					v.Name = t.Value
				}
			}
			res.Instances = append(res.Instances, v)
		}
	}
	return res, nil
}

type stateChange struct {
	ID   string `json:"id"`
	From string `json:"from,omitempty"`
	To   string `json:"to"`
}

type idsArgs struct {
	InstanceIDs []string `json:"instance_ids"`
}

// ec2Change starts, stops or reboots instances. They are looked up in the
// client's region only, so instances elsewhere are not found.
func ec2Change(action string) func(context.Context, client, idsArgs) (any, error) {
	return func(ctx context.Context, c client, in idsArgs) (any, error) {
		if len(in.InstanceIDs) == 0 {
			return nil, apitool.Need("instance_ids", "")
		}
		p := url.Values{"Action": {action}}
		if err := addIDs(p, in.InstanceIDs); err != nil {
			return nil, err
		}
		var out struct {
			Items []struct {
				ID   string `xml:"instanceId"`
				From string `xml:"previousState>name"`
				To   string `xml:"currentState>name"`
			} `xml:"instancesSet>item"`
		}
		if err := c.query(ctx, "ec2", ec2Version, p, &out); err != nil {
			return nil, err
		}
		res := []stateChange{}
		if action == "RebootInstances" {
			// EC2 answers a reboot with only true.
			for _, id := range in.InstanceIDs {
				res = append(res, stateChange{ID: id, To: "rebooting"})
			}
			return res, nil
		}
		for _, i := range out.Items {
			res = append(res, stateChange{ID: i.ID, From: i.From, To: i.To})
		}
		return res, nil
	}
}
