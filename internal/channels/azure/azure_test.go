package azure

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/bespinian/keera-agents/internal/channels"
)

const (
	sub   = "11111111-2222-3333-4444-555555555555"
	other = "99999999-2222-3333-4444-555555555555"
	vm    = "/subscriptions/" + sub + "/resourceGroups/web/providers/Microsoft.Compute/virtualMachines/vm1"
)

type fake struct {
	srv       *httptest.Server
	last      *http.Request
	body      map[string]any
	providers atomic.Int32
	noSubs    bool
}

func newFake(t *testing.T) (*fake, *Adapter) {
	f := &fake{}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /login/tenant-1/oauth2/v2.0/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("client_secret") != "pw" || r.Form.Get("scope") != scope {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"invalid_client","error_description":"AADSTS7000215: Invalid client secret provided."}`))
			return
		}
		_, _ = w.Write([]byte(`{"access_token":"tok","expires_in":3600}`))
	})
	reply := func(status int, body string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer tok" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			f.last, f.body = r, nil
			_ = json.NewDecoder(r.Body).Decode(&f.body)
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
		}
	}
	s := "/arm/subscriptions/" + sub
	mux.HandleFunc("GET /arm/subscriptions", func(w http.ResponseWriter, r *http.Request) {
		if f.noSubs {
			reply(200, `{"value":[]}`)(w, r)
			return
		}
		reply(200, `{"value":[{"subscriptionId":"`+sub+`"}]}`)(w, r)
	})
	mux.HandleFunc("GET "+s, reply(200, `{"subscriptionId":"`+sub+`","state":"Enabled"}`))
	mux.HandleFunc("GET /arm/subscriptions/"+other, reply(404,
		`{"error":{"code":"SubscriptionNotFound","message":"The subscription could not be found."}}`))
	mux.HandleFunc("GET "+s+"/resourcegroups", reply(200, `{"value":[{"id":"`+s[4:]+`/resourceGroups/web","name":"web","location":"westeurope","properties":{"provisioningState":"Succeeded"}}]}`))
	mux.HandleFunc("GET "+s+"/resourceGroups/web/resources", reply(200, `{"value":[{"id":"`+vm+`","name":"vm1","type":"Microsoft.Compute/virtualMachines","location":"westeurope","tags":{"env":"prod"},"properties":{"big":true}}],
		"nextLink":"`+"https://elsewhere.example/steal"+`"}`))
	mux.HandleFunc("GET "+s+"/providers/Microsoft.Compute", func(w http.ResponseWriter, r *http.Request) {
		f.providers.Add(1)
		reply(200, `{"namespace":"Microsoft.Compute","resourceTypes":[
			{"resourceType":"disks","apiVersions":["2020-01-01"]},
			{"resourceType":"virtualMachines","apiVersions":["2025-01-01-preview","2024-07-01","2023-09-01"]}]}`)(w, r)
	})
	mux.HandleFunc("GET /arm"+vm, reply(200, `{"id":"`+vm+`","name":"vm1","type":"Microsoft.Compute/virtualMachines","location":"westeurope",
		"properties":{"instanceView":{"statuses":[{"code":"PowerState/running"}]}}}`))
	mux.HandleFunc("GET /arm"+strings.Replace(vm, "vm1", "gone", 1), reply(404,
		`{"error":{"code":"ResourceNotFound","message":"The Resource 'Microsoft.Compute/virtualMachines/gone' under resource group 'web' was not found."}}`))
	mux.HandleFunc("GET "+s+"/providers/Microsoft.Insights/eventtypes/management/values", reply(200, `{"value":[
		{"eventTimestamp":"2026-11-04T07:00:00Z","operationName":{"value":"Microsoft.Compute/virtualMachines/deallocate/action","localizedValue":"Deallocate Virtual Machine"},
		 "status":{"value":"Succeeded"},"caller":"ada@example.com","level":"Informational","resourceId":"`+vm+`"}]}`))
	mux.HandleFunc("GET /arm"+vm+"/providers/Microsoft.Insights/metrics", reply(200, `{"value":[{"name":{"value":"Percentage CPU"},"unit":"Percent",
		"timeseries":[{"data":[{"timeStamp":"2026-11-04T06:00:00Z","average":12.5},{"timeStamp":"2026-11-04T07:00:00Z"}]}]}]}`))
	mux.HandleFunc("GET /arm"+vm+"/providers/Microsoft.Insights/metricDefinitions", reply(200, `{"value":[{"name":{"value":"Percentage CPU"},"unit":"Percent","primaryAggregationType":"Average"}]}`))
	mux.HandleFunc("POST "+s+"/providers/Microsoft.CostManagement/query", reply(200, `{"properties":{
		"columns":[{"name":"Cost","type":"Number"},{"name":"ResourceGroupName","type":"String"},{"name":"Currency","type":"String"}],
		"rows":[[1.234,"db","CHF"],[10.5,"web","CHF"]]}}`))
	mux.HandleFunc("POST /arm"+vm+"/deallocate", reply(202, ``))
	mux.HandleFunc("POST /arm"+strings.Replace(vm, "vm1", "locked", 1)+"/start", reply(403,
		`{"error":{"code":"AuthorizationFailed","message":"The client does not have authorization to perform action 'Microsoft.Compute/virtualMachines/start/action'."}}`))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected %s %s", r.Method, r.URL)
		w.WriteHeader(http.StatusNotFound)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	a := New(channels.Options{Endpoints: map[string]string{"login": f.srv.URL + "/login", "arm": f.srv.URL + "/arm"}})
	return f, a
}

func conn(secret string) channels.Connection {
	return channels.Connection{
		Kind:    channels.Azure,
		Config:  map[string]string{"tenant_id": "tenant-1", "client_id": "app"},
		Secrets: map[string]string{"client_secret": secret},
	}
}

func call[T any](t *testing.T, a *Adapter, tool, args string) T {
	t.Helper()
	out, err := a.Call(context.Background(), conn("pw"), sub, tool, json.RawMessage(args))
	if err != nil {
		t.Fatalf("%s: %v", tool, err)
	}
	v, ok := out.(T)
	if !ok {
		t.Fatalf("%s returned %T", tool, out)
	}
	return v
}

func TestCheck(t *testing.T) {
	f, a := newFake(t)
	ctx := context.Background()
	if err := a.Check(ctx, conn("pw")); err != nil {
		t.Fatal(err)
	}
	if err := a.Check(ctx, conn("wrong")); err == nil || !strings.Contains(err.Error(), "invalid_client") {
		t.Errorf("wrong secret: %v", err)
	}
	f.noSubs = true
	if err := a.Check(ctx, conn("pw")); err == nil || !strings.Contains(err.Error(), "Reader role") {
		t.Errorf("no subscriptions: %v", err)
	}
}

func TestTarget(t *testing.T) {
	_, a := newFake(t)
	ctx := context.Background()
	if got, err := a.Target(ctx, conn("pw"), " "+strings.ToUpper(sub)+" "); err != nil || got != sub {
		t.Fatalf("Target = %q, %v", got, err)
	}
	if _, err := a.Target(ctx, conn("pw"), other); err == nil || !strings.Contains(err.Error(), "Reader role") {
		t.Errorf("unseen subscription: %v", err)
	}
	for _, bad := range []string{"*", "", "prod", sub + "/resourceGroups/x"} {
		if _, err := a.Target(ctx, conn("pw"), bad); err == nil {
			t.Errorf("%q passed", bad)
		}
	}
}

func TestResources(t *testing.T) {
	f, a := newFake(t)
	groups := call[[]Resource](t, a, "list_resource_groups", `{}`)
	if len(groups) != 1 || groups[0].Name != "web" {
		t.Fatalf("groups = %+v", groups)
	}
	list := call[[]Resource](t, a, "list_resources", `{"resource_group":"web","type":"Microsoft.Compute/virtualMachines","limit":1}`)
	if len(list) != 1 || list[0].Tags["env"] != "prod" || list[0].Properties != nil {
		t.Fatalf("resources = %+v", list)
	}
	if q := f.last.URL.Query(); q.Get("$filter") != "resourceType eq 'Microsoft.Compute/virtualMachines'" || q.Get("$top") != "1" {
		t.Errorf("query = %s", f.last.URL.RawQuery)
	}
	if _, err := a.Call(context.Background(), conn("pw"), sub, "list_resources", json.RawMessage(`{"type":"x' or 1 eq 1"}`)); err == nil {
		t.Error("filter injection passed")
	}

	for range 2 {
		r := call[Resource](t, a, "get_resource", `{"id":"`+strings.ToUpper(vm[:52])+vm[52:]+`"}`)
		if r.Name != "vm1" || !strings.Contains(string(r.Properties.(json.RawMessage)), "PowerState/running") {
			t.Fatalf("resource = %+v", r)
		}
		if q := f.last.URL.Query(); q.Get("api-version") != "2024-07-01" || q.Get("$expand") != "instanceView" {
			t.Errorf("query = %s", f.last.URL.RawQuery)
		}
	}
	if f.providers.Load() != 1 {
		t.Errorf("API versions looked up %d times", f.providers.Load())
	}
	_, err := a.Call(context.Background(), conn("pw"), sub, "get_resource", json.RawMessage(`{"id":"`+strings.Replace(vm, "vm1", "gone", 1)+`"}`))
	if err == nil || !strings.Contains(err.Error(), "Azure answered 404 Not Found ResourceNotFound") {
		t.Errorf("missing resource: %v", err)
	}
}

// Every id must be in the bound subscription, and nothing in it may
// change the URL beyond naming a resource.
func TestStaysInSubscription(t *testing.T) {
	_, a := newFake(t)
	ctx := context.Background()
	for _, id := range []string{
		strings.Replace(vm, sub, other, 1),
		"/subscriptions/" + sub + "/../" + other + "/resourceGroups/web",
		"/subscriptions/" + sub + "x/resourceGroups/web",
		vm + "?api-version=1",
		vm + "/../../../../" + other,
		"subscriptions/" + sub,
		"https://management.azure.com" + vm,
	} {
		for _, tool := range []string{"get_resource", "vm_start"} {
			if _, err := a.Call(ctx, conn("pw"), sub, tool, json.RawMessage(`{"id":`+quote(id)+`}`)); err == nil {
				t.Errorf("%s %q passed", tool, id)
			}
		}
		if _, err := a.Call(ctx, conn("pw"), sub, "metrics", json.RawMessage(`{"resource_id":`+quote(id)+`,"metrics":["x"]}`)); err == nil {
			t.Errorf("metrics %q passed", id)
		}
	}
	if _, err := a.Call(ctx, conn("pw"), channels.AnyTarget, "list_resources", nil); err == nil {
		t.Error("any target passed")
	}
	// A next page elsewhere is not followed with the token.
	if _, err := a.Call(ctx, conn("pw"), sub, "list_resources", json.RawMessage(`{"resource_group":"web","limit":10}`)); err == nil ||
		!strings.Contains(err.Error(), "does not point to Azure") {
		t.Errorf("foreign next link: %v", err)
	}
}

func quote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func TestMonitoring(t *testing.T) {
	f, a := newFake(t)
	events := call[[]Event](t, a, "activity_log", `{"resource_group":"web"}`)
	if len(events) != 1 || events[0].Operation != "Deallocate Virtual Machine" || events[0].Caller != "ada@example.com" {
		t.Fatalf("events = %+v", events)
	}
	if fl := f.last.URL.Query().Get("$filter"); !strings.Contains(fl, "eventTimestamp ge '") || !strings.HasSuffix(fl, "resourceGroupName eq 'web'") {
		t.Errorf("filter = %s", fl)
	}

	series := call[[]Series](t, a, "metrics", `{"resource_id":"`+vm+`","metrics":["Percentage CPU"],"interval":"pt5m","aggregation":"average"}`)
	if len(series) != 1 || len(series[0].Points) != 1 || series[0].Points[0].Value != 12.5 {
		t.Fatalf("series = %+v", series)
	}
	if q := f.last.URL.Query(); q.Get("metricnames") != "Percentage CPU" || q.Get("interval") != "PT5M" || !strings.Contains(q.Get("timespan"), "/") {
		t.Errorf("query = %s", f.last.URL.RawQuery)
	}
	names := call[map[string]any](t, a, "metrics", `{"resource_id":"`+vm+`"}`)
	if names["metrics"] == nil {
		t.Errorf("names = %v", names)
	}
	if _, err := a.Call(context.Background(), conn("pw"), sub, "metrics",
		json.RawMessage(`{"resource_id":"`+vm+`","metrics":["x"],"interval":"5 minutes"}`)); err == nil {
		t.Error("bad interval passed")
	}
}

func TestCosts(t *testing.T) {
	f, a := newFake(t)
	c := call[Costs](t, a, "costs", `{"start":"2026-10-01","end":"2026-10-31","group_by":"resource_group"}`)
	if c.Currency != "CHF" || c.Total != 11.73 || len(c.Rows) != 2 || c.Rows[0].Group != "web" || c.Rows[1].Cost != 1.23 {
		t.Fatalf("costs = %+v", c)
	}
	period := f.body["timePeriod"].(map[string]any)
	ds := f.body["dataset"].(map[string]any)
	if f.body["type"] != "ActualCost" || period["to"] != "2026-10-31T23:59:59Z" ||
		ds["grouping"].([]any)[0].(map[string]any)["name"] != "ResourceGroupName" {
		t.Errorf("sent %v", f.body)
	}
	if _, err := a.Call(context.Background(), conn("pw"), sub, "costs", json.RawMessage(`{"group_by":"tag"}`)); err == nil {
		t.Error("unknown grouping passed")
	}
}

func TestVMs(t *testing.T) {
	_, a := newFake(t)
	out := call[map[string]any](t, a, "vm_stop", `{"id":"`+vm+`"}`)
	if out["accepted"] != true || out["action"] != "deallocate" || out["vm"] != "vm1" {
		t.Errorf("stop = %v", out)
	}
	_, err := a.Call(context.Background(), conn("pw"), sub, "vm_start", json.RawMessage(`{"id":"`+strings.Replace(vm, "vm1", "locked", 1)+`"}`))
	if err == nil || !strings.Contains(err.Error(), "Virtual Machine Contributor") || !strings.Contains(err.Error(), "AuthorizationFailed") {
		t.Errorf("forbidden: %v", err)
	}
	disk := "/subscriptions/" + sub + "/resourceGroups/web/providers/Microsoft.Compute/disks/d1"
	if _, err := a.Call(context.Background(), conn("pw"), sub, "vm_restart", json.RawMessage(`{"id":"`+disk+`"}`)); err == nil {
		t.Error("a disk restarted")
	}
	for _, tool := range a.Tools() {
		if want := strings.HasPrefix(tool.Name, "vm_"); tool.Write != want {
			t.Errorf("%s: Write = %v", tool.Name, tool.Write)
		}
	}
}
