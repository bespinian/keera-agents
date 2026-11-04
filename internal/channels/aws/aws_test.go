package aws

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bespinian/keera-agents/internal/channels"
	"github.com/bespinian/keera-agents/internal/channels/internal/web"
)

const roleARN = "arn:aws:iam::123456789012:role/agents"

// keys are the access keys the fake knows. ASIATEST is the session it hands
// out for roleARN.
var keys = map[string]creds{
	"AKIDTEST":  {secret: "secret"},
	"AKIDNOEC2": {secret: "secret"},
	"ASIATEST":  {secret: "sessionsecret", token: "session-token"},
}

type fake struct {
	t       *testing.T
	srv     *httptest.Server
	now     time.Time
	calls   []string
	params  map[string]url.Values
	bodies  map[string]map[string]any
	puts    map[string]string
	assumed int
}

func newFake(t *testing.T) (*fake, *Adapter) {
	f := &fake{
		t: t, now: time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC),
		params: map[string]url.Values{}, bodies: map[string]map[string]any{}, puts: map[string]string{},
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	a := New(channels.Options{Endpoints: map[string]string{"aws": f.srv.URL}})
	a.now = func() time.Time { return f.now }
	return f, a
}

func conn(id, secret, role string) channels.Connection {
	return channels.Connection{
		Kind:    channels.AWS,
		Config:  map[string]string{"access_key_id": id, "role_arn": role},
		Secrets: map[string]string{"secret_access_key": secret},
	}
}

var good = conn("AKIDTEST", "secret", "")

func xmlError(w http.ResponseWriter, status int, code, msg string) {
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, `<ErrorResponse><Error><Type>Sender</Type><Code>%s</Code><Message>%s</Message></Error></ErrorResponse>`, code, msg)
}

// verify checks a request's signature as AWS does, and returns the key's
// id, region and service.
func (f *fake) verify(w http.ResponseWriter, r *http.Request, body []byte) (string, string, string, bool) {
	auth, ok := strings.CutPrefix(r.Header.Get("Authorization"), algorithm+" ")
	parts := map[string]string{}
	for p := range strings.SplitSeq(auth, ", ") {
		k, v, _ := strings.Cut(p, "=")
		parts[k] = v
	}
	cred := strings.Split(parts["Credential"], "/")
	if !ok || len(cred) != 5 {
		xmlError(w, http.StatusForbidden, "MissingAuthenticationToken", "unsigned")
		return "", "", "", false
	}
	key, known := keys[cred[0]]
	if !known || r.Header.Get("X-Amz-Security-Token") != key.token {
		xmlError(w, http.StatusForbidden, "InvalidClientTokenId", "The security token included in the request is invalid.")
		return "", "", "", false
	}
	region, service := cred[2], cred[3]
	at, _ := time.Parse(amzDate, r.Header.Get("X-Amz-Date"))
	want := signature(r, strings.Split(parts["SignedHeaders"], ";"), hexSHA256(body), key.secret, region, service, at)
	if parts["Signature"] != want || service == "s3" && r.Header.Get("X-Amz-Content-Sha256") != hexSHA256(body) {
		xmlError(w, http.StatusForbidden, "SignatureDoesNotMatch", "The request signature we calculated does not match the signature you provided.")
		return "", "", "", false
	}
	return cred[0], region, service, true
}

func (f *fake) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	id, region, service, ok := f.verify(w, r, body)
	if !ok {
		return
	}
	switch service {
	case "s3":
		f.calls = append(f.calls, "s3 "+region+" "+r.Method+" "+r.URL.Path)
		f.s3(w, r, body)
	case "logs", "ce":
		target := r.Header.Get("X-Amz-Target")
		f.calls = append(f.calls, service+" "+region+" "+target)
		var in map[string]any
		_ = json.Unmarshal(body, &in)
		f.bodies[target] = in
		f.json(w, target, in)
	default:
		form, _ := url.ParseQuery(string(body))
		action := form.Get("Action")
		f.calls = append(f.calls, service+" "+region+" "+action)
		f.params[action] = form
		f.query(w, id, region, action, form)
	}
}

func (f *fake) query(w http.ResponseWriter, id, region, action string, form url.Values) {
	switch action {
	case "GetCallerIdentity":
		arn := "arn:aws:iam::123456789012:user/agents"
		if id == "ASIATEST" {
			arn = "arn:aws:sts::123456789012:assumed-role/agents/keera-agents"
		}
		_, _ = fmt.Fprintf(w, `<GetCallerIdentityResponse><GetCallerIdentityResult><Arn>%s</Arn><UserId>X</UserId>`+
			`<Account>123456789012</Account></GetCallerIdentityResult></GetCallerIdentityResponse>`, arn)
	case "AssumeRole":
		if form.Get("RoleArn") != roleARN {
			xmlError(w, http.StatusForbidden, "AccessDenied", "User is not authorized to perform: sts:AssumeRole")
			return
		}
		f.assumed++
		_, _ = fmt.Fprintf(w, `<AssumeRoleResponse><AssumeRoleResult><Credentials><AccessKeyId>ASIATEST</AccessKeyId>`+
			`<SecretAccessKey>sessionsecret</SecretAccessKey><SessionToken>session-token</SessionToken>`+
			`<Expiration>%s</Expiration></Credentials></AssumeRoleResult></AssumeRoleResponse>`, f.now.Add(time.Hour).Format(time.RFC3339))
	case "DescribeRegions", "DescribeInstances", "StartInstances", "StopInstances", "RebootInstances":
		if id == "AKIDNOEC2" {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`<Response><Errors><Error><Code>UnauthorizedOperation</Code><Message>You are not authorized.</Message></Error></Errors></Response>`))
			return
		}
		f.ec2(w, region, action, form)
	case "DescribeAlarms":
		_, _ = w.Write([]byte(`<DescribeAlarmsResponse xmlns="http://monitoring.amazonaws.com/doc/2010-08-01/"><DescribeAlarmsResult><MetricAlarms>
			<member><AlarmName>api-5xx</AlarmName><StateValue>ALARM</StateValue><StateReason>Threshold Crossed</StateReason>
			<Namespace>AWS/ApplicationELB</Namespace><MetricName>HTTPCode_Target_5XX_Count</MetricName>
			<StateUpdatedTimestamp>2026-11-04T09:30:00.000Z</StateUpdatedTimestamp></member>
			</MetricAlarms></DescribeAlarmsResult></DescribeAlarmsResponse>`))
	case "GetMetricStatistics":
		_, _ = w.Write([]byte(`<GetMetricStatisticsResponse><GetMetricStatisticsResult><Datapoints>
			<member><Timestamp>2026-11-04T09:30:00Z</Timestamp><Maximum>80.5</Maximum><Unit>Percent</Unit></member>
			<member><Timestamp>2026-11-04T09:00:00Z</Timestamp><Maximum>12.25</Maximum><Unit>Percent</Unit></member>
			</Datapoints><Label>CPUUtilization</Label></GetMetricStatisticsResult></GetMetricStatisticsResponse>`))
	default:
		f.t.Errorf("unexpected action %s", action)
		xmlError(w, http.StatusBadRequest, "InvalidAction", action)
	}
}

func (f *fake) ec2(w http.ResponseWriter, region, action string, form url.Values) {
	ec2Error := func(code, msg string) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = fmt.Fprintf(w, `<Response><Errors><Error><Code>%s</Code><Message>%s</Message></Error></Errors><RequestID>r</RequestID></Response>`, code, msg)
	}
	switch action {
	case "DescribeRegions":
		items := ""
		if name := form.Get("RegionName.1"); name == region && (name == "eu-central-1" || name == "us-east-1") {
			items = "<item><regionName>" + name + "</regionName><optInStatus>opt-in-not-required</optInStatus></item>"
		}
		_, _ = w.Write([]byte(`<DescribeRegionsResponse xmlns="http://ec2.amazonaws.com/doc/2016-11-15/"><regionInfo>` + items + `</regionInfo></DescribeRegionsResponse>`))
	case "DescribeInstances":
		if form.Get("InstanceId.1") == "i-00000000000000000" {
			ec2Error("InvalidInstanceID.NotFound", "The instance ID 'i-00000000000000000' does not exist")
			return
		}
		_, _ = w.Write([]byte(`<DescribeInstancesResponse xmlns="http://ec2.amazonaws.com/doc/2016-11-15/"><reservationSet>
			<item><reservationId>r-1</reservationId><instancesSet>
				<item><instanceId>i-0abcd1234ef567890</instanceId><instanceState><code>16</code><name>running</name></instanceState>
				<instanceType>t3.micro</instanceType><placement><availabilityZone>eu-central-1a</availabilityZone></placement>
				<privateIpAddress>10.0.0.5</privateIpAddress><ipAddress>3.120.0.1</ipAddress><launchTime>2026-10-01T08:00:00.000Z</launchTime>
				<tagSet><item><key>env</key><value>prod</value></item><item><key>Name</key><value>web-1</value></item></tagSet></item>
			</instancesSet></item>
			<item><reservationId>r-2</reservationId><instancesSet>
				<item><instanceId>i-0123456789abcdef0</instanceId><instanceState><code>80</code><name>stopped</name></instanceState>
				<instanceType>t3.large</instanceType><placement><availabilityZone>eu-central-1b</availabilityZone></placement>
				<privateIpAddress>10.0.1.7</privateIpAddress><launchTime>2026-09-01T08:00:00.000Z</launchTime></item>
			</instancesSet></item>
			</reservationSet></DescribeInstancesResponse>`))
	case "StartInstances", "StopInstances":
		from, to := "stopped", "pending"
		if action == "StopInstances" {
			from, to = "running", "stopping"
		}
		_, _ = fmt.Fprintf(w, `<%[1]sResponse><instancesSet><item><instanceId>%[2]s</instanceId>`+
			`<currentState><code>0</code><name>%[4]s</name></currentState><previousState><code>80</code><name>%[3]s</name></previousState>`+
			`</item></instancesSet></%[1]sResponse>`, action, form.Get("InstanceId.1"), from, to)
	case "RebootInstances":
		_, _ = w.Write([]byte(`<RebootInstancesResponse><return>true</return></RebootInstancesResponse>`))
	}
}

func (f *fake) s3(w http.ResponseWriter, r *http.Request, body []byte) {
	regions := map[string]string{"logs-eu": "eu-central-1", "far": "us-west-2"}
	bucket, key, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	if bucket == "far" && r.Method != http.MethodHead {
		f.t.Errorf("reached a bucket outside the region: %s %s", r.Method, r.URL)
	}
	switch {
	case r.Method == http.MethodHead:
		region, ok := regions[bucket]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("X-Amz-Bucket-Region", region)
		if bucket == "far" {
			w.WriteHeader(http.StatusMovedPermanently)
		}
	case r.Method == http.MethodGet && bucket == "":
		f.params["ListBuckets"] = r.URL.Query()
		_, _ = w.Write([]byte(`<ListAllMyBucketsResult><Buckets>
			<Bucket><Name>logs-eu</Name><CreationDate>2025-01-02T03:04:05.000Z</CreationDate><BucketRegion>eu-central-1</BucketRegion></Bucket>
			</Buckets><Owner><ID>o</ID></Owner></ListAllMyBucketsResult>`))
	case r.Method == http.MethodGet && key == "":
		f.params["ListObjectsV2"] = r.URL.Query()
		_, _ = w.Write([]byte(`<ListBucketResult><Name>logs-eu</Name><IsTruncated>true</IsTruncated>
			<Contents><Key>reports/a.csv</Key><LastModified>2026-11-01T00:00:00.000Z</LastModified><Size>42</Size><StorageClass>STANDARD</StorageClass></Contents>
			<CommonPrefixes><Prefix>reports/2026/</Prefix></CommonPrefixes>
			<NextContinuationToken>tok2</NextContinuationToken></ListBucketResult>`))
	case r.Method == http.MethodGet:
		switch key {
		case "notes/a b+c.txt":
			w.Header().Set("Content-Type", "text/plain")
			w.Header().Set("Last-Modified", "Mon, 02 Nov 2026 10:00:00 GMT")
			_, _ = w.Write([]byte("hello"))
		case "big.log":
			// Answers Range, as S3 does.
			http.ServeContent(w, r, key, time.Time{}, bytes.NewReader(bytes.Repeat([]byte("a"), 2<<20)))
		case "blob.bin":
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write([]byte{0, 1, 2})
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?><Error><Code>NoSuchKey</Code><Message>The specified key does not exist.</Message><Key>x</Key></Error>`))
		}
	case r.Method == http.MethodPut:
		f.puts[key] = string(body)
		f.puts[key+" type"] = r.Header.Get("Content-Type")
		w.Header().Set("ETag", `"abc"`)
	default:
		f.t.Errorf("unexpected %s %s", r.Method, r.URL)
	}
}

func (f *fake) json(w http.ResponseWriter, target string, in map[string]any) {
	switch target {
	case "Logs_20140328.FilterLogEvents":
		if in["logGroupName"] == "/missing" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"__type":"com.amazonaws.logs#ResourceNotFoundException","message":"The specified log group does not exist."}`))
			return
		}
		if in["nextToken"] == nil {
			_, _ = w.Write([]byte(`{"events":[{"logStreamName":"s1","timestamp":1793786400000,"message":"ERROR boom\n"}],"nextToken":"p2"}`))
			return
		}
		_, _ = w.Write([]byte(`{"events":[{"logStreamName":"s2","timestamp":1793786460000,"message":"ERROR again"}]}`))
	case "AWSInsightsIndexService.GetCostAndUsage":
		_, _ = w.Write([]byte(`{"ResultsByTime":[{"TimePeriod":{"Start":"2026-11-01","End":"2026-11-05"},"Total":{},
			"Groups":[{"Keys":["AWS Lambda"],"Metrics":{"UnblendedCost":{"Amount":"1.004","Unit":"USD"}}},
			{"Keys":["Amazon Elastic Compute Cloud - Compute"],"Metrics":{"UnblendedCost":{"Amount":"12.5","Unit":"USD"}}}],
			"Estimated":true}]}`))
	default:
		f.t.Errorf("unexpected target %s", target)
	}
}

func call(t *testing.T, a *Adapter, c channels.Connection, target, tool, args string) (any, error) {
	t.Helper()
	return a.Call(context.Background(), c, target, tool, json.RawMessage(args))
}

// asJSON is what the model sees.
func asJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestCheck(t *testing.T) {
	f, a := newFake(t)
	ctx := context.Background()
	if err := a.Check(ctx, good); err != nil {
		t.Fatal(err)
	}
	if err := a.Check(ctx, conn("AKIDTEST", "wrong", "")); err == nil || !strings.Contains(err.Error(), "secret access key does not belong") {
		t.Fatalf("wrong secret: %v", err)
	}
	if err := a.Check(ctx, conn("AKIDNOBODY", "secret", "")); err == nil || !strings.Contains(err.Error(), "does not know the access key ID") {
		t.Fatalf("unknown key: %v", err)
	}
	if err := a.Check(ctx, conn("", "secret", "")); err == nil {
		t.Fatal("no access key passed")
	}
	if err := a.Check(ctx, conn("AKIDTEST", "secret", "not-a-role")); err == nil || !strings.Contains(err.Error(), "role's ARN") {
		t.Fatalf("bad role ARN: %v", err)
	}
	if err := a.Check(ctx, conn("AKIDTEST", "secret", roleARN)); err != nil || f.assumed != 1 {
		t.Fatalf("with role: %v, assumed %d", err, f.assumed)
	}
	err := a.Check(ctx, conn("AKIDTEST", "secret", "arn:aws:iam::123456789012:role/other"))
	if err == nil || !strings.Contains(err.Error(), "arn:aws:iam::123456789012:user/agents may not assume") {
		t.Fatalf("role not trusted: %v", err)
	}
	// The secret is never in an error.
	if err := a.Check(ctx, conn("AKIDTEST", "wrong", "")); strings.Contains(err.Error(), "wrong") {
		t.Fatalf("error holds the secret: %v", err)
	}
	if !slices.Contains(f.calls, "sts us-east-1 GetCallerIdentity") {
		t.Fatalf("calls = %v", f.calls)
	}
}

func TestSessionIsCached(t *testing.T) {
	f, a := newFake(t)
	c := conn("AKIDTEST", "secret", roleARN)
	for range 2 {
		if _, err := call(t, a, c, "eu-central-1", "whoami", `{}`); err != nil {
			t.Fatal(err)
		}
	}
	f.now = f.now.Add(30 * time.Minute)
	out, err := call(t, a, c, "eu-central-1", "whoami", `{}`)
	if err != nil || f.assumed != 1 || !strings.Contains(asJSON(t, out), "assumed-role/agents") {
		t.Fatalf("whoami = %v, %v, assumed %d", out, err, f.assumed)
	}
	// Shortly before the session ends, a new one is taken.
	f.now = f.now.Add(26 * time.Minute)
	if _, err := call(t, a, c, "eu-central-1", "whoami", `{}`); err != nil || f.assumed != 2 {
		t.Fatalf("renewal: %v, assumed %d", err, f.assumed)
	}
}

func TestTarget(t *testing.T) {
	f, a := newFake(t)
	ctx := context.Background()
	if r, err := a.Target(ctx, good, " EU-Central-1 "); err != nil || r != "eu-central-1" {
		t.Fatalf("Target = %q, %v", r, err)
	}
	if !slices.Contains(f.calls, "ec2 eu-central-1 DescribeRegions") {
		t.Fatalf("calls = %v", f.calls)
	}
	if _, err := a.Target(ctx, good, "ap-east-1"); err == nil || !strings.Contains(err.Error(), "not enabled") {
		t.Fatalf("disabled region: %v", err)
	}
	for _, bad := range []string{"", "*", "mars", "eu-central", "eu-central-1/../x"} {
		if _, err := a.Target(ctx, good, bad); err == nil || !strings.Contains(err.Error(), "not an AWS region") {
			t.Errorf("%q: %v", bad, err)
		}
	}
	// Without EC2 permissions, STS in the region answers instead.
	if r, err := a.Target(ctx, conn("AKIDNOEC2", "secret", ""), "eu-central-1"); err != nil || r != "eu-central-1" {
		t.Fatalf("Target without EC2 = %q, %v", r, err)
	}
	if !slices.Contains(f.calls, "sts eu-central-1 GetCallerIdentity") {
		t.Fatalf("calls = %v", f.calls)
	}
}

func TestEC2(t *testing.T) {
	f, a := newFake(t)
	out, err := call(t, a, good, "eu-central-1", "ec2_instances",
		`{"filters":{"tag:Name":["web-*"],"instance-state-name":["running","stopped"]},"limit":1}`)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"instances":[{"id":"i-0abcd1234ef567890","name":"web-1","state":"running","type":"t3.micro","zone":"eu-central-1a",` +
		`"private_ip":"10.0.0.5","public_ip":"3.120.0.1","launched":"2026-10-01T08:00:00Z"}],"more":true}`
	if got := asJSON(t, out); got != want {
		t.Fatalf("ec2_instances =\n%s\nwant\n%s", got, want)
	}
	p := f.params["DescribeInstances"]
	if p.Get("MaxResults") != "5" || p.Get("Filter.1.Name") != "instance-state-name" || p.Get("Filter.1.Value.2") != "stopped" ||
		p.Get("Filter.2.Name") != "tag:Name" || p.Get("Version") != ec2Version {
		t.Fatalf("params = %v", p)
	}
	if !slices.Contains(f.calls, "ec2 eu-central-1 DescribeInstances") {
		t.Fatalf("calls = %v", f.calls)
	}

	for tool, want := range map[string]string{
		"ec2_start":  `[{"id":"i-0123456789abcdef0","from":"stopped","to":"pending"}]`,
		"ec2_stop":   `[{"id":"i-0123456789abcdef0","from":"running","to":"stopping"}]`,
		"ec2_reboot": `[{"id":"i-0123456789abcdef0","to":"rebooting"}]`,
	} {
		out, err := call(t, a, good, "eu-central-1", tool, `{"instance_ids":["i-0123456789abcdef0"]}`)
		if err != nil || asJSON(t, out) != want {
			t.Errorf("%s = %s, %v", tool, asJSON(t, out), err)
		}
	}
	for _, args := range []string{`{}`, `{"instance_ids":["web-1"]}`} {
		if _, err := call(t, a, good, "eu-central-1", "ec2_stop", args); err == nil {
			t.Errorf("ec2_stop %s passed", args)
		}
	}
}

func TestAPIErrors(t *testing.T) {
	_, a := newFake(t)
	_, err := call(t, a, good, "eu-central-1", "ec2_instances", `{"instance_ids":["i-00000000000000000"]}`)
	if err == nil || err.Error() != "Amazon EC2 answered 400 Bad Request InvalidInstanceID.NotFound: The instance ID 'i-00000000000000000' does not exist" {
		t.Fatalf("EC2 error: %v", err)
	}
	_, err = call(t, a, good, "eu-central-1", "logs_search", `{"log_group":"/missing"}`)
	if err == nil || err.Error() != "CloudWatch Logs answered 400 Bad Request ResourceNotFoundException: The specified log group does not exist." {
		t.Fatalf("Logs error: %v", err)
	}
	_, err = call(t, a, good, "eu-central-1", "s3_read", `{"bucket":"logs-eu","key":"gone.txt"}`)
	if err == nil || err.Error() != "Amazon S3 answered 404 Not Found NoSuchKey: The specified key does not exist." {
		t.Fatalf("S3 error: %v", err)
	}
	if _, err := call(t, a, good, "eu-central-1", "nope", `{}`); err == nil {
		t.Fatal("unknown tool passed")
	}
	if _, err := call(t, a, good, channels.AnyTarget, "whoami", `{}`); err == nil {
		t.Fatal("any target passed")
	}
}

func TestS3(t *testing.T) {
	f, a := newFake(t)
	out, err := call(t, a, good, "eu-central-1", "s3_buckets", `{}`)
	if err != nil || asJSON(t, out) != `{"buckets":[{"name":"logs-eu","created":"2025-01-02T03:04:05Z"}]}` {
		t.Fatalf("s3_buckets = %s, %v", asJSON(t, out), err)
	}
	if q := f.params["ListBuckets"]; q.Get("bucket-region") != "eu-central-1" || q.Get("max-buckets") != "25" {
		t.Fatalf("ListBuckets query = %v", q)
	}

	out, err = call(t, a, good, "eu-central-1", "s3_list", `{"bucket":"logs-eu","prefix":"reports/","limit":500}`)
	want := `{"folders":["reports/2026/"],"objects":[{"key":"reports/a.csv","size":42,"modified":"2026-11-01T00:00:00Z"}],"next":"tok2"}`
	if err != nil || asJSON(t, out) != want {
		t.Fatalf("s3_list = %s, %v", asJSON(t, out), err)
	}
	if q := f.params["ListObjectsV2"]; q.Get("delimiter") != "/" || q.Get("prefix") != "reports/" || q.Get("max-keys") != "100" {
		t.Fatalf("ListObjectsV2 query = %v", q)
	}

	out, err = call(t, a, good, "eu-central-1", "s3_read", `{"bucket":"logs-eu","key":"notes/a b+c.txt"}`)
	want = `{"bucket":"logs-eu","key":"notes/a b+c.txt","content_type":"text/plain","modified":"2026-11-02T10:00:00Z","offset":0,"size":5,"content":"hello"}`
	if err != nil || asJSON(t, out) != want {
		t.Fatalf("s3_read = %s, %v", asJSON(t, out), err)
	}
	// A long object is read a chunk at a time.
	out, err = call(t, a, good, "eu-central-1", "s3_read", `{"bucket":"logs-eu","key":"big.log"}`)
	if c, ok := out.(content); err != nil || !ok || len(c.Content) != web.ChunkBytes || c.Next != web.ChunkBytes || c.Size != 2<<20 {
		t.Fatalf("big object: %v", err)
	}
	out, err = call(t, a, good, "eu-central-1", "s3_read", fmt.Sprintf(`{"bucket":"logs-eu","key":"big.log","offset":%d}`, 2<<20-10))
	if c, ok := out.(content); err != nil || !ok || c.Content != "aaaaaaaaaa" || c.Next != 0 {
		t.Fatalf("end of big object: %+v, %v", out, err)
	}
	if _, err := call(t, a, good, "eu-central-1", "s3_read", `{"bucket":"logs-eu","key":"blob.bin"}`); err == nil ||
		!strings.Contains(err.Error(), "is not text") {
		t.Fatalf("binary object: %v", err)
	}

	out, err = call(t, a, good, "eu-central-1", "s3_write", `{"bucket":"logs-eu","key":"out/r.csv","content":"a,b\n","content_type":"text/csv"}`)
	if err != nil || asJSON(t, out) != `{"bucket":"logs-eu","key":"out/r.csv","size":4,"etag":"abc"}` {
		t.Fatalf("s3_write = %s, %v", asJSON(t, out), err)
	}
	if f.puts["out/r.csv"] != "a,b\n" || f.puts["out/r.csv type"] != "text/csv" {
		t.Fatalf("puts = %v", f.puts)
	}
	// The bucket's region was asked once.
	heads := 0
	for _, c := range f.calls {
		if c == "s3 eu-central-1 HEAD /logs-eu" {
			heads++
		}
	}
	if heads != 1 {
		t.Fatalf("calls = %v", f.calls)
	}
}

func TestS3StaysInRegion(t *testing.T) {
	f, a := newFake(t)
	for _, tool := range []string{"s3_list", "s3_read", "s3_write"} {
		_, err := call(t, a, good, "eu-central-1", tool, `{"bucket":"far","key":"x","content":"y"}`)
		if err == nil || err.Error() != "the bucket far is in us-west-2, outside this agent's region, eu-central-1" {
			t.Errorf("%s: %v", tool, err)
		}
	}
	if _, err := call(t, a, good, "eu-central-1", "s3_list", `{"bucket":"nobody"}`); err == nil || !strings.Contains(err.Error(), "no S3 bucket") {
		t.Fatalf("missing bucket: %v", err)
	}
	if _, err := call(t, a, good, "eu-central-1", "s3_read", `{"bucket":"../etc","key":"x"}`); err == nil || !strings.Contains(err.Error(), "not an S3 bucket") {
		t.Fatalf("bad bucket: %v", err)
	}
	if len(f.puts) != 0 {
		t.Fatalf("wrote %v", f.puts)
	}
}

func TestCloudWatch(t *testing.T) {
	f, a := newFake(t)
	out, err := call(t, a, good, "eu-central-1", "cloudwatch_alarms", `{"state":"ALARM"}`)
	want := `[{"name":"api-5xx","state":"ALARM","reason":"Threshold Crossed","namespace":"AWS/ApplicationELB",` +
		`"metric":"HTTPCode_Target_5XX_Count","updated":"2026-11-04T09:30:00Z"}]`
	if err != nil || asJSON(t, out) != want {
		t.Fatalf("cloudwatch_alarms = %s, %v", asJSON(t, out), err)
	}
	if p := f.params["DescribeAlarms"]; p.Get("StateValue") != "ALARM" || p.Get("MaxRecords") != "25" {
		t.Fatalf("DescribeAlarms = %v", p)
	}

	out, err = call(t, a, good, "eu-central-1", "cloudwatch_metrics",
		`{"namespace":"AWS/EC2","metric_name":"CPUUtilization","dimensions":{"InstanceId":"i-0abcd1234ef567890"},"statistic":"Maximum","start":"6h"}`)
	want = `{"namespace":"AWS/EC2","metric":"CPUUtilization","statistic":"Maximum","unit":"Percent","period":360,` +
		`"points":[{"at":"2026-11-04T09:00:00Z","value":12.25},{"at":"2026-11-04T09:30:00Z","value":80.5}]}`
	if err != nil || asJSON(t, out) != want {
		t.Fatalf("cloudwatch_metrics = %s, %v", asJSON(t, out), err)
	}
	p := f.params["GetMetricStatistics"]
	if p.Get("StartTime") != "2026-11-04T04:00:00Z" || p.Get("EndTime") != "2026-11-04T10:00:00Z" ||
		p.Get("Dimensions.member.1.Value") != "i-0abcd1234ef567890" || p.Get("Statistics.member.1") != "Maximum" {
		t.Fatalf("GetMetricStatistics = %v", p)
	}
	if !slices.Contains(f.calls, "monitoring eu-central-1 GetMetricStatistics") {
		t.Fatalf("calls = %v", f.calls)
	}
	for _, args := range []string{
		`{"namespace":"AWS/EC2","metric_name":"CPUUtilization","period":60,"start":"7d"}`,
		`{"namespace":"AWS/EC2","metric_name":"CPUUtilization","statistic":"p99"}`,
		`{"namespace":"AWS/EC2","metric_name":"CPUUtilization","start":"yesterday"}`,
	} {
		if _, err := call(t, a, good, "eu-central-1", "cloudwatch_metrics", args); err == nil {
			t.Errorf("%s passed", args)
		}
	}
}

func TestLogs(t *testing.T) {
	f, a := newFake(t)
	out, err := call(t, a, good, "eu-central-1", "logs_search", `{"log_group":"/aws/lambda/checkout","filter_pattern":"ERROR"}`)
	want := `{"events":[{"at":"2026-11-04T10:00:00Z","stream":"s1","message":"ERROR boom"},{"at":"2026-11-04T10:01:00Z","stream":"s2","message":"ERROR again"}]}`
	if err != nil || asJSON(t, out) != want {
		t.Fatalf("logs_search = %s, %v", asJSON(t, out), err)
	}
	in := f.bodies["Logs_20140328.FilterLogEvents"]
	if in["filterPattern"] != "ERROR" || in["startTime"] != float64(f.now.Add(-time.Hour).UnixMilli()) || in["limit"] != float64(24) {
		t.Fatalf("FilterLogEvents = %v", in)
	}
	if !slices.Contains(f.calls, "logs eu-central-1 Logs_20140328.FilterLogEvents") {
		t.Fatalf("calls = %v", f.calls)
	}
	if _, err := call(t, a, good, "eu-central-1", "logs_search", `{"log_group":"arn:aws:logs:us-west-2:123456789012:log-group:x"}`); err == nil {
		t.Fatal("an ARN passed")
	}
}

func TestCosts(t *testing.T) {
	f, a := newFake(t)
	out, err := call(t, a, good, "eu-central-1", "costs", `{"group_by":"SERVICE"}`)
	want := `[{"start":"2026-11-01","end":"2026-11-05","amount":13.5,"unit":"USD","estimated":true,` +
		`"groups":[{"key":"Amazon Elastic Compute Cloud - Compute","amount":12.5},{"key":"AWS Lambda","amount":1}]}]`
	if err != nil || asJSON(t, out) != want {
		t.Fatalf("costs = %s, %v", asJSON(t, out), err)
	}
	in := asJSON(t, f.bodies["AWSInsightsIndexService.GetCostAndUsage"])
	for _, s := range []string{
		`"TimePeriod":{"End":"2026-11-05","Start":"2026-11-01"}`,
		`"Filter":{"Dimensions":{"Key":"REGION","Values":["eu-central-1"]}}`,
		`"GroupBy":[{"Key":"SERVICE","Type":"DIMENSION"}]`,
		`"Granularity":"MONTHLY"`,
	} {
		if !strings.Contains(in, s) {
			t.Errorf("GetCostAndUsage = %s, without %s", in, s)
		}
	}
	// Cost Explorer has only us-east-1.
	if !slices.Contains(f.calls, "ce us-east-1 AWSInsightsIndexService.GetCostAndUsage") {
		t.Fatalf("calls = %v", f.calls)
	}
	for _, args := range []string{`{"group_by":"REGION"}`, `{"start":"2026-11-05","end":"2026-11-01"}`, `{"granularity":"DAILY","start":"2026-01-01"}`} {
		if _, err := call(t, a, good, "eu-central-1", "costs", args); err == nil {
			t.Errorf("%s passed", args)
		}
	}
}

func TestDescriptor(t *testing.T) {
	_, a := newFake(t)
	d := a.Descriptor()
	if d.Kind != channels.AWS || d.AnyTarget || d.TargetLabel != "Region" || strings.Contains(d.Setup, "{webhook}") {
		t.Fatalf("descriptor = %+v", d)
	}
	var writes []string
	for _, tool := range a.Tools() {
		if tool.Write {
			writes = append(writes, tool.Name)
		}
	}
	if want := []string{"ec2_start", "ec2_stop", "ec2_reboot", "s3_write"}; !slices.Equal(writes, want) {
		t.Fatalf("write tools = %v, want %v", writes, want)
	}
}
