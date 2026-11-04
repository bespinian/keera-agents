package kubernetes

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/bespinian/keera-agents/internal/channels"
)

type fake struct {
	srv *httptest.Server
	ca  string

	mu   sync.Mutex
	sent map[string]sent
}

// sent is a request with a body, as the fake received it.
type sent struct {
	query       string
	contentType string
	body        map[string]any
}

func newFake(t *testing.T) *fake {
	f := &fake{sent: map[string]sent{}}
	mux := http.NewServeMux()
	reply := func(status int, body string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer tok" {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"kind":"Status","message":"Unauthorized","reason":"Unauthorized","code":401}`))
				return
			}
			var in map[string]any
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &in)
			f.mu.Lock()
			f.sent[r.Method+" "+r.URL.Path] = sent{query: r.URL.RawQuery, contentType: r.Header.Get("Content-Type"), body: in}
			f.mu.Unlock()
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
		}
	}
	forbidden := `{"kind":"Status","message":"namespaces \"scoped\" is forbidden","reason":"Forbidden","code":403}`
	mux.HandleFunc("POST /apis/authentication.k8s.io/v1/selfsubjectreviews", reply(201, `{"status":{"userInfo":{"username":"sa"}}}`))
	mux.HandleFunc("GET /api/v1/namespaces/shop", reply(200, `{"metadata":{"name":"shop"}}`))
	mux.HandleFunc("GET /api/v1/namespaces/scoped", reply(403, forbidden))
	mux.HandleFunc("GET /api/v1/namespaces/scoped/pods", reply(200, `{"items":[]}`))
	mux.HandleFunc("GET /api/v1/namespaces/locked", reply(403, forbidden))
	mux.HandleFunc("GET /api/v1/namespaces/locked/pods", reply(403, forbidden))
	mux.HandleFunc("GET /api/v1", reply(200, `{"resources":[
		{"name":"pods","kind":"Pod","namespaced":true},
		{"name":"pods/log","kind":"Pod","namespaced":true},
		{"name":"secrets","kind":"Secret","namespaced":true},
		{"name":"configmaps","kind":"ConfigMap","namespaced":true},
		{"name":"namespaces","kind":"Namespace","namespaced":false}]}`))
	mux.HandleFunc("GET /apis/apps/v1", reply(200, `{"resources":[{"name":"deployments","kind":"Deployment","namespaced":true}]}`))
	mux.HandleFunc("GET /api/v1/namespaces/shop/pods", reply(200, `{"metadata":{"continue":"x"},"items":[{
		"metadata":{"name":"web-1","creationTimestamp":"2020-01-01T00:00:00Z"},
		"spec":{"containers":[{"name":"web"},{"name":"proxy"}]},
		"status":{"phase":"Running","containerStatuses":[
			{"ready":true,"restartCount":0,"state":{"running":{}}},
			{"ready":false,"restartCount":7,"state":{"waiting":{"reason":"CrashLoopBackOff"}}}]}}]}`))
	mux.HandleFunc("GET /apis/apps/v1/namespaces/shop/deployments/web", reply(200,
		`{"kind":"Deployment","metadata":{"name":"web","managedFields":[{"manager":"kubectl"}]},"spec":{"replicas":2}}`))
	mux.HandleFunc("GET /apis/apps/v1/namespaces/shop/deployments/gone", reply(404,
		`{"kind":"Status","message":"deployments.apps \"gone\" not found","reason":"NotFound","code":404}`))
	mux.HandleFunc("GET /api/v1/namespaces/shop/secrets/db", reply(200, `{"kind":"Secret","type":"Opaque",
		"metadata":{"name":"db","annotations":{"kubectl.kubernetes.io/last-applied-configuration":"{\"data\":{\"password\":\"aHVudGVyMg==\"}}"}},
		"data":{"password":"aHVudGVyMg==","user":"YWRh"}}`))
	mux.HandleFunc("GET /api/v1/namespaces/shop/pods/web-1/log", reply(200, "started\nlistening on :8080\n"))
	mux.HandleFunc("GET /api/v1/namespaces/shop/events", reply(200, `{"items":[
		{"involvedObject":{"kind":"Pod","name":"web-1"},"type":"Normal","reason":"Pulled","lastTimestamp":"2024-01-01T10:00:00Z"},
		{"involvedObject":{"kind":"Pod","name":"web-1"},"type":"Warning","reason":"BackOff","lastTimestamp":"2024-01-01T11:00:00Z","count":5}]}`))
	mux.HandleFunc("PATCH /apis/apps/v1/namespaces/shop/deployments/web", reply(200,
		`{"metadata":{"name":"web"},"spec":{"replicas":2},"status":{"readyReplicas":1}}`))
	mux.HandleFunc("PATCH /api/v1/namespaces/shop/secrets/db", reply(200,
		`{"type":"Opaque","metadata":{"name":"db"},"data":{"password":"aHVudGVyMg=="}}`))
	mux.HandleFunc("PATCH /apis/apps/v1/namespaces/shop/deployments/web/scale", reply(200, `{}`))
	mux.HandleFunc("DELETE /api/v1/namespaces/shop/configmaps/old", reply(200, `{}`))
	mux.HandleFunc("/", reply(404, `{"kind":"Status","message":"the server could not find the requested resource","code":404}`))
	f.srv = httptest.NewTLSServer(mux)
	t.Cleanup(f.srv.Close)
	f.ca = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: f.srv.Certificate().Raw}))
	return f
}

func (f *fake) conn(token string) channels.Connection {
	return channels.Connection{
		Kind:    channels.Kubernetes,
		Config:  map[string]string{"server": f.srv.URL, "ca_cert": f.ca},
		Secrets: map[string]string{"token": token},
	}
}

func (f *fake) got(key string) sent {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sent[key]
}

func call(t *testing.T, a *Adapter, conn channels.Connection, target, tool, args string) (string, error) {
	t.Helper()
	out, err := a.Call(context.Background(), conn, target, tool, json.RawMessage(args))
	if err != nil {
		return "", err
	}
	if s, ok := out.(string); ok {
		return s, nil
	}
	b, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	return string(b), nil
}

func TestCheck(t *testing.T) {
	f := newFake(t)
	a := New(channels.Options{})
	ctx := context.Background()
	if err := a.Check(ctx, f.conn("tok")); err != nil {
		t.Fatal(err)
	}
	if err := a.Check(ctx, f.conn("wrong")); err == nil || !strings.Contains(err.Error(), "does not accept the token") {
		t.Fatalf("wrong token: %v", err)
	}
	// Without the cluster's CA, its certificate is not trusted.
	noCA := f.conn("tok")
	noCA.Config["ca_cert"] = ""
	if err := a.Check(ctx, noCA); err == nil || !strings.Contains(err.Error(), "certificate") {
		t.Fatalf("untrusted certificate: %v", err)
	}
	badCA := f.conn("tok")
	badCA.Config["ca_cert"] = "not a certificate"
	if err := a.Check(ctx, badCA); err == nil || !strings.Contains(err.Error(), "PEM") {
		t.Fatalf("bad CA: %v", err)
	}
	plain := f.conn("tok")
	plain.Config["server"] = "http://k8s.example.com"
	if err := a.Check(ctx, plain); err == nil || !strings.Contains(err.Error(), "https://") {
		t.Fatalf("plain http: %v", err)
	}
}

func TestTarget(t *testing.T) {
	f := newFake(t)
	a := New(channels.Options{})
	ctx := context.Background()
	if ns, err := a.Target(ctx, f.conn("tok"), " Shop "); err != nil || ns != "shop" {
		t.Fatalf("Target = %q, %v", ns, err)
	}
	if ns, err := a.Target(ctx, f.conn("tok"), "scoped"); err != nil || ns != "scoped" {
		t.Fatalf("namespace-scoped token: %q, %v", ns, err)
	}
	if _, err := a.Target(ctx, f.conn("tok"), "locked"); err == nil || !strings.Contains(err.Error(), "may not read namespace locked") {
		t.Fatalf("locked: %v", err)
	}
	if _, err := a.Target(ctx, f.conn("tok"), "nope"); err == nil || !strings.Contains(err.Error(), "no namespace nope") {
		t.Fatalf("missing: %v", err)
	}
	for _, bad := range []string{"*", "a/b", "-x", "../etc", strings.Repeat("a", 64)} {
		if _, err := a.Target(ctx, f.conn("tok"), bad); err == nil {
			t.Errorf("%q passed", bad)
		}
	}
}

func TestReadTools(t *testing.T) {
	f := newFake(t)
	a := New(channels.Options{})
	c := f.conn("tok")

	out, err := call(t, a, c, "shop", "list", `{"kind":"pod","limit":500}`)
	if err != nil || !strings.Contains(out, `"status":"CrashLoopBackOff","ready":"1/2","restarts":7`) || !strings.Contains(out, `"more":true`) {
		t.Fatalf("list = %s, %v", out, err)
	}
	if q := f.got("GET /api/v1/namespaces/shop/pods").query; q != "limit=100" {
		t.Errorf("list query = %s", q)
	}

	out, err = call(t, a, c, "shop", "get", `{"api_version":"apps/v1","kind":"Deployment","name":"web"}`)
	if err != nil || strings.Contains(out, "managedFields") || !strings.Contains(out, `"replicas":2`) {
		t.Fatalf("get = %s, %v", out, err)
	}
	out, err = call(t, a, c, "shop", "get", `{"kind":"Secret","name":"db"}`)
	if err != nil || strings.Contains(out, "aHVudGVyMg") || !strings.Contains(out, `"data_keys":["password","user"]`) {
		t.Fatalf("secret = %s, %v", out, err)
	}

	out, err = call(t, a, c, "shop", "logs", `{"pod":"web-1","previous":true}`)
	if err != nil || !strings.Contains(out, "listening on :8080") {
		t.Fatalf("logs = %s, %v", out, err)
	}
	if q := f.got("GET /api/v1/namespaces/shop/pods/web-1/log").query; !strings.Contains(q, "tailLines=200") || !strings.Contains(q, "previous=true") {
		t.Errorf("logs query = %s", q)
	}

	out, err = call(t, a, c, "shop", "events", `{"name":"web-1"}`)
	if err != nil || strings.Index(out, "BackOff") > strings.Index(out, "Pulled") {
		t.Fatalf("events, newest first = %s, %v", out, err)
	}
	if q := f.got("GET /api/v1/namespaces/shop/events").query; q != "fieldSelector=involvedObject.name%3Dweb-1" {
		t.Errorf("events query = %s", q)
	}
}

func TestWriteTools(t *testing.T) {
	f := newFake(t)
	a := New(channels.Options{})
	c := f.conn("tok")

	out, err := call(t, a, c, "shop", "apply", `{"manifest":{"apiVersion":"apps/v1","kind":"deployment","metadata":{"name":"web"},"spec":{"replicas":2}}}`)
	if err != nil || !strings.Contains(out, `"done":"applied"`) || !strings.Contains(out, `"ready":"1/2"`) {
		t.Fatalf("apply = %s, %v", out, err)
	}
	s := f.got("PATCH /apis/apps/v1/namespaces/shop/deployments/web")
	md, _ := s.body["metadata"].(map[string]any)
	if s.contentType != "application/apply-patch+yaml" || s.query != "fieldManager=keera-agents&force=true" ||
		md["namespace"] != "shop" || s.body["kind"] != "Deployment" {
		t.Errorf("apply sent %+v", s)
	}
	out, err = call(t, a, c, "shop", "apply", `{"manifest":{"apiVersion":"v1","kind":"Secret","metadata":{"name":"db"},"stringData":{"password":"hunter2"}}}`)
	if err != nil || strings.Contains(out, "aHVudGVyMg") || strings.Contains(out, "hunter2") {
		t.Fatalf("apply secret = %s, %v", out, err)
	}
	if _, err := call(t, a, c, "shop", "apply", `{"manifest":"apiVersion: v1\nkind: Pod"}`); err == nil || !strings.Contains(err.Error(), "YAML") {
		t.Errorf("YAML manifest: %v", err)
	}

	if _, err := call(t, a, c, "shop", "scale", `{"kind":"Deployment","name":"web","replicas":3}`); err != nil {
		t.Fatal(err)
	}
	s = f.got("PATCH /apis/apps/v1/namespaces/shop/deployments/web/scale")
	if spec, _ := s.body["spec"].(map[string]any); spec["replicas"] != 3.0 || s.contentType != "application/merge-patch+json" {
		t.Errorf("scale sent %+v", s)
	}
	if _, err := call(t, a, c, "shop", "scale", `{"kind":"Deployment","name":"web"}`); err == nil {
		t.Error("scale without replicas passed")
	}

	if _, err := call(t, a, c, "shop", "restart", `{"kind":"Deployment","name":"web"}`); err != nil {
		t.Fatal(err)
	}
	s = f.got("PATCH /apis/apps/v1/namespaces/shop/deployments/web")
	if !strings.Contains(s.contentType, "merge-patch") || field(s.body, "spec", "template", "metadata", "annotations", "kubectl.kubernetes.io/restartedAt") == nil {
		t.Errorf("restart sent %+v", s)
	}

	if out, err := call(t, a, c, "shop", "delete", `{"kind":"ConfigMap","name":"old"}`); err != nil || !strings.Contains(out, "deleted") {
		t.Fatalf("delete = %s, %v", out, err)
	}
}

func TestStaysInNamespace(t *testing.T) {
	f := newFake(t)
	a := New(channels.Options{})
	c := f.conn("tok")
	cases := []struct{ target, tool, args, want string }{
		{"shop", "apply", `{"manifest":{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"x","namespace":"kube-system"}}}`, "only within shop"},
		{"shop", "get", `{"kind":"Namespace","api_version":"v1","name":"kube-system"}`, "not in a namespace"},
		{"shop", "delete", `{"kind":"Namespace","api_version":"v1","name":"shop"}`, "not in a namespace"},
		{"shop", "logs", `{"pod":"../../kube-system/pods/x"}`, "not a valid pod"},
		{"*", "list", `{"kind":"Pod"}`, "not a namespace"},
	}
	for _, tc := range cases {
		if _, err := call(t, a, c, tc.target, tc.tool, tc.args); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s %s: %v", tc.tool, tc.args, err)
		}
	}
}

func TestAPIError(t *testing.T) {
	f := newFake(t)
	a := New(channels.Options{})
	_, err := call(t, a, f.conn("tok"), "shop", "get", `{"kind":"Deployment","name":"gone"}`)
	if err == nil || err.Error() != `Kubernetes answered 404 Not Found: deployments.apps "gone" not found` {
		t.Fatalf("err = %v", err)
	}
	if _, err := call(t, a, f.conn("tok"), "shop", "list", `{"api_version":"example.com/v1","kind":"Widget"}`); err == nil ||
		!strings.Contains(err.Error(), "does not serve apiVersion example.com/v1") {
		t.Fatalf("unknown group: %v", err)
	}
}

func TestTools(t *testing.T) {
	writes := map[string]bool{}
	for _, tool := range New(channels.Options{}).Tools() {
		writes[tool.Name] = tool.Write
	}
	want := map[string]bool{"list": false, "get": false, "logs": false, "events": false, "apply": true, "delete": true, "scale": true, "restart": true}
	if len(writes) != len(want) {
		t.Fatalf("tools = %v", writes)
	}
	for name, w := range want {
		if writes[name] != w {
			t.Errorf("%s: write = %v", name, writes[name])
		}
	}
}
