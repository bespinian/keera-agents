package gcp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/bespinian/keera-agents/internal/channels"
	"github.com/bespinian/keera-agents/internal/channels/internal/jwttest"
)

type fake struct {
	srv *httptest.Server
	key string

	mu   sync.Mutex
	seen map[string]string // path to query or body
	// bucketLookups counts reads of a bucket's project, which are cached.
	bucketLookups atomic.Int32
}

func newFake(t *testing.T) (*fake, *Adapter) {
	f := &fake{seen: map[string]string{}, key: jwttest.NewKey(t, "k").ServiceAccountJSON("sa@acme-prod.iam.gserviceaccount.com")}
	mux := http.NewServeMux()
	reply := func(status int, body string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer g" {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error":{"message":"no token","status":"UNAUTHENTICATED"}}`))
				return
			}
			b, _ := io.ReadAll(r.Body)
			f.mu.Lock()
			f.seen[r.Method+" "+r.URL.EscapedPath()] = r.URL.RawQuery + string(b)
			f.mu.Unlock()
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
		}
	}
	denied := `{"error":{"code":403,"message":"The caller does not have permission","status":"PERMISSION_DENIED"}}`
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"access_token":"g","expires_in":3600}`))
	})
	mux.HandleFunc("GET /rm/v3/projects/acme-prod", reply(200, `{"name":"projects/111","projectId":"acme-prod","state":"ACTIVE"}`))
	mux.HandleFunc("GET /rm/v3/projects/other-proj", reply(403, denied))
	mux.HandleFunc("GET /rm/v3/projects/old-project", reply(200, `{"name":"projects/222","state":"DELETE_REQUESTED"}`))
	mux.HandleFunc("GET /asset/v1/projects/acme-prod:searchAllResources", reply(200, `{"results":[
		{"name":"//storage.googleapis.com/acme-data","assetType":"storage.googleapis.com/Bucket","location":"eu","project":"projects/111","labels":{"env":"prod"}}]}`))
	mux.HandleFunc("GET /compute/compute/v1/projects/acme-prod/aggregated/instances", reply(200, `{"items":{
		"zones/us-east1-b":{"warning":{"code":"NO_RESULTS_ON_PAGE"}},
		"zones/europe-west6-a":{"instances":[{"name":"web-1","status":"RUNNING",
			"zone":"https://www.googleapis.com/compute/v1/projects/acme-prod/zones/europe-west6-a",
			"machineType":"https://www.googleapis.com/compute/v1/projects/acme-prod/zones/europe-west6-a/machineTypes/e2-small",
			"networkInterfaces":[{"networkIP":"10.0.0.2","accessConfigs":[{"natIP":"34.1.2.3"}]}]}]}}}`))
	mux.HandleFunc("POST /logging/v2/entries:list", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(strings.NewReader(string(b)))
		if strings.Contains(string(b), "bad") {
			reply(400, `{"error":{"code":400,"message":"Unparseable filter: syntax error at line 1","status":"INVALID_ARGUMENT"}}`)(w, r)
			return
		}
		reply(200, `{"entries":[{"timestamp":"2024-01-01T10:00:00Z","severity":"ERROR",
			"logName":"projects/acme-prod/logs/cloudaudit.googleapis.com%2Factivity",
			"resource":{"type":"gce_instance","labels":{"instance_id":"1"}},
			"jsonPayload":{"message":"`+strings.Repeat("x", 3000)+`"}}]}`)(w, r)
	})
	mux.HandleFunc("GET /monitoring/v3/projects/acme-prod/timeSeries", reply(200, `{"timeSeries":[{
		"metric":{"type":"compute.googleapis.com/instance/cpu/utilization"},"resource":{"type":"gce_instance","labels":{"zone":"europe-west6-a"}},
		"points":[{"interval":{"endTime":"2024-01-01T10:05:00Z"},"value":{"doubleValue":0.5}},
			{"interval":{"endTime":"2024-01-01T10:00:00Z"},"value":{"int64Value":"7"}}]}]}`))
	mux.HandleFunc("GET /storage/storage/v1/b", reply(200, `{"items":[{"name":"acme-data","location":"EU","storageClass":"STANDARD"}]}`))
	mux.HandleFunc("GET /storage/storage/v1/b/acme-data", func(w http.ResponseWriter, r *http.Request) {
		f.bucketLookups.Add(1)
		reply(200, `{"projectNumber":"111"}`)(w, r)
	})
	mux.HandleFunc("GET /storage/storage/v1/b/foreign", reply(200, `{"projectNumber":"999"}`))
	mux.HandleFunc("GET /storage/storage/v1/b/acme-data/o", reply(200, `{"prefixes":["reports/"],"items":[
		{"name":"readme.txt","size":"12","contentType":"text/plain"}],"nextPageToken":"n"}`))
	mux.HandleFunc("GET /storage/storage/v1/b/acme-data/o/", func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.EscapedPath() == "/storage/storage/v1/b/acme-data/o/reports%2Fq1.csv" && r.URL.Query().Get("alt") == "media":
			reply(206, "region,total\neu,3\n")(w, r)
		case r.URL.EscapedPath() == "/storage/storage/v1/b/acme-data/o/reports%2Fq1.csv":
			reply(200, `{"size":"17","contentType":"text/csv"}`)(w, r)
		case r.URL.EscapedPath() == "/storage/storage/v1/b/acme-data/o/logo.png":
			reply(200, `{"size":"900","contentType":"image/png"}`)(w, r)
		default:
			reply(404, `{"error":{"code":404,"message":"No such object"}}`)(w, r)
		}
	})
	mux.HandleFunc("POST /compute/compute/v1/projects/acme-prod/zones/europe-west6-a/instances/web-1/stop", reply(200,
		`{"name":"operation-1","status":"RUNNING"}`))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected %s %s", r.Method, r.URL)
		w.WriteHeader(http.StatusNotFound)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	ep := map[string]string{"token": f.srv.URL + "/token"}
	for _, name := range []string{"asset", "compute", "logging", "monitoring", "storage"} {
		ep[name] = f.srv.URL + "/" + name
	}
	ep["resourcemanager"] = f.srv.URL + "/rm"
	return f, New(channels.Options{HTTP: f.srv.Client(), Endpoints: ep})
}

func (f *fake) conn() channels.Connection {
	return channels.Connection{Kind: channels.GCP, Secrets: map[string]string{"service_account_json": f.key}}
}

func (f *fake) got(key string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.seen[key]
}

func call(t *testing.T, a *Adapter, conn channels.Connection, target, tool, args string) (string, error) {
	t.Helper()
	out, err := a.Call(context.Background(), conn, target, tool, json.RawMessage(args))
	if err != nil {
		return "", err
	}
	b, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	return string(b), nil
}

func TestCheck(t *testing.T) {
	f, a := newFake(t)
	ctx := context.Background()
	if err := a.Check(ctx, f.conn()); err != nil {
		t.Fatal(err)
	}
	if err := a.Check(ctx, channels.Connection{}); err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("no key: %v", err)
	}
	bad := channels.Connection{Secrets: map[string]string{"service_account_json": "{}"}}
	if err := a.Check(ctx, bad); err == nil {
		t.Fatal("empty key passed")
	}
}

func TestTarget(t *testing.T) {
	f, a := newFake(t)
	ctx := context.Background()
	if id, err := a.Target(ctx, f.conn(), " Acme-Prod "); err != nil || id != "acme-prod" {
		t.Fatalf("Target = %q, %v", id, err)
	}
	if _, err := a.Target(ctx, f.conn(), "other-proj"); err == nil || !strings.Contains(err.Error(), "cannot see project other-proj") {
		t.Fatalf("no access: %v", err)
	}
	if _, err := a.Target(ctx, f.conn(), "old-project"); err == nil || !strings.Contains(err.Error(), "delete requested") {
		t.Fatalf("deleted project: %v", err)
	}
	for _, bad := range []string{"*", "ab", "1project", "acme/prod", "acme-prod-"} {
		if _, err := a.Target(ctx, f.conn(), bad); err == nil {
			t.Errorf("%q passed", bad)
		}
	}
}

func TestReadTools(t *testing.T) {
	f, a := newFake(t)
	c := f.conn()

	out, err := call(t, a, c, "acme-prod", "search_resources", `{"query":"labels.env:prod","asset_types":["storage.googleapis.com/Bucket"]}`)
	if err != nil || !strings.Contains(out, `"name":"storage.googleapis.com/acme-data"`) || !strings.Contains(out, `"labels":{"env":"prod"}`) {
		t.Fatalf("search_resources = %s, %v", out, err)
	}
	if q := f.got("GET /asset/v1/projects/acme-prod:searchAllResources"); !strings.Contains(q, "assetTypes=storage.googleapis.com%2FBucket") {
		t.Errorf("search query = %s", q)
	}

	out, err = call(t, a, c, "acme-prod", "list_instances", `{}`)
	want := `[{"name":"web-1","zone":"europe-west6-a","status":"RUNNING","machine_type":"e2-small","internal_ips":["10.0.0.2"],"external_ips":["34.1.2.3"]}]`
	if err != nil || out != want {
		t.Fatalf("list_instances = %s, %v", out, err)
	}

	out, err = call(t, a, c, "acme-prod", "logs", `{"filter":"severity>=ERROR"}`)
	if err != nil || !strings.Contains(out, `"log":"cloudaudit.googleapis.com/activity"`) || len(out) > 2500 {
		t.Fatalf("logs = %s, %v", out, err)
	}
	sent := f.got("POST /logging/v2/entries:list")
	var body struct {
		ResourceNames []string `json:"resourceNames"`
		Filter        string   `json:"filter"`
		OrderBy       string   `json:"orderBy"`
	}
	_ = json.Unmarshal([]byte(sent), &body)
	if len(body.ResourceNames) != 1 || body.ResourceNames[0] != "projects/acme-prod" || body.OrderBy != "timestamp desc" ||
		!strings.HasPrefix(body.Filter, "(severity>=ERROR) AND timestamp >= ") {
		t.Errorf("logs sent %s", sent)
	}

	out, err = call(t, a, c, "acme-prod", "metrics", `{"filter":"metric.type = \"compute.googleapis.com/instance/cpu/utilization\"","alignment_period_seconds":300}`)
	if err != nil || !strings.Contains(out, `"value":0.5`) || !strings.Contains(out, `"value":7`) {
		t.Fatalf("metrics = %s, %v", out, err)
	}
	if q := f.got("GET /monitoring/v3/projects/acme-prod/timeSeries"); !strings.Contains(q, "aggregation.alignmentPeriod=300s") ||
		!strings.Contains(q, "aggregation.perSeriesAligner=ALIGN_MEAN") || !strings.Contains(q, "interval.startTime=") {
		t.Errorf("metrics query = %s", q)
	}

	out, err = call(t, a, c, "acme-prod", "list_buckets", `{}`)
	if err != nil || !strings.Contains(out, `"name":"acme-data"`) {
		t.Fatalf("list_buckets = %s, %v", out, err)
	}
	if q := f.got("GET /storage/storage/v1/b"); !strings.Contains(q, "project=acme-prod") {
		t.Errorf("buckets query = %s", q)
	}

	out, err = call(t, a, c, "acme-prod", "list_objects", `{"bucket":"acme-data"}`)
	if err != nil || !strings.Contains(out, `"folders":["reports/"]`) || !strings.Contains(out, `"size":12`) || !strings.Contains(out, `"more":true`) {
		t.Fatalf("list_objects = %s, %v", out, err)
	}
	out, err = call(t, a, c, "acme-prod", "read_object", `{"bucket":"acme-data","name":"reports/q1.csv"}`)
	if err != nil || !strings.Contains(out, `"content":"region,total\neu,3\n"`) || strings.Contains(out, `"next_offset"`) {
		t.Fatalf("read_object = %s, %v", out, err)
	}
	if n := f.bucketLookups.Load(); n != 1 {
		t.Errorf("bucket's project read %d times", n)
	}
	if _, err := call(t, a, c, "acme-prod", "read_object", `{"bucket":"acme-data","name":"logo.png"}`); err == nil || !strings.Contains(err.Error(), "not text") {
		t.Errorf("image: %v", err)
	}
}

func TestInstanceStop(t *testing.T) {
	f, a := newFake(t)
	out, err := call(t, a, f.conn(), "acme-prod", "instance_stop", `{"zone":"europe-west6-a","name":"web-1"}`)
	if err != nil || out != `{"instance":"web-1","zone":"europe-west6-a","done":"stopping","operation":"operation-1","status":"RUNNING"}` {
		t.Fatalf("instance_stop = %s, %v", out, err)
	}
	if _, err := call(t, a, f.conn(), "acme-prod", "instance_start", `{"zone":"../x","name":"web-1"}`); err == nil {
		t.Error("bad zone passed")
	}
}

func TestStaysInProject(t *testing.T) {
	f, a := newFake(t)
	c := f.conn()
	cases := []struct{ target, tool, args, want string }{
		{"acme-prod", "list_objects", `{"bucket":"foreign"}`, "not in project acme-prod"},
		{"acme-prod", "read_object", `{"bucket":"foreign","name":"x"}`, "not in project acme-prod"},
		{"acme-prod", "read_object", `{"bucket":"../b/foreign","name":"x"}`, "not a bucket name"},
		{"*", "list_buckets", `{}`, "not a project ID"},
		{"*", "logs", `{}`, "not a project ID"},
	}
	for _, tc := range cases {
		if _, err := call(t, a, c, tc.target, tc.tool, tc.args); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s %s: %v", tc.tool, tc.args, err)
		}
	}
}

func TestAPIError(t *testing.T) {
	f, a := newFake(t)
	_, err := call(t, a, f.conn(), "acme-prod", "logs", `{"filter":"bad"}`)
	if err == nil || err.Error() != "Google answered 400 Bad Request INVALID_ARGUMENT: Unparseable filter: syntax error at line 1" {
		t.Fatalf("err = %v", err)
	}
}

func TestTools(t *testing.T) {
	writes := map[string]bool{}
	for _, tool := range New(channels.Options{}).Tools() {
		writes[tool.Name] = tool.Write
	}
	if len(writes) != 9 || !writes["instance_start"] || !writes["instance_stop"] || writes["read_object"] {
		t.Fatalf("tools = %v", writes)
	}
}
