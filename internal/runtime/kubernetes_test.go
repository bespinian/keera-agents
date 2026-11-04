package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// The driver against a stand-in API server that answers in the API's shapes.

type fakeAPI struct {
	srv  *httptest.Server
	mu   sync.Mutex
	objs map[string]json.RawMessage
	seen []request
}

type request struct {
	Method      string
	Path        string
	Query       string
	ContentType string
	Raw         string
	Body        map[string]any
}

func notFound(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusNotFound)
	_, _ = w.Write([]byte(`{"message":"not found","reason":"NotFound","code":404}`))
}

func newFakeAPI(t *testing.T) *fakeAPI {
	t.Helper()
	f := &fakeAPI{objs: map[string]json.RawMessage{}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		raw, _ := io.ReadAll(r.Body)
		rec := request{
			Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery,
			ContentType: r.Header.Get("Content-Type"), Raw: string(raw),
		}
		_ = json.Unmarshal(raw, &rec.Body)
		f.seen = append(f.seen, rec)

		switch r.Method {
		case http.MethodGet:
			if strings.HasSuffix(r.URL.Path, "/statefulsets") {
				_, _ = w.Write([]byte(`{"items":[]}`))
				return
			}
			if before, ok := strings.CutSuffix(r.URL.Path, "/log"); ok {
				if _, ok := f.objs[before]; !ok {
					notFound(w)
					return
				}
				_, _ = w.Write([]byte("hello from the runtime\n"))
				return
			}
			body, ok := f.objs[r.URL.Path]
			if !ok {
				notFound(w)
				return
			}
			_, _ = w.Write(body)
		case http.MethodPost:
			name, _ := rec.Body["metadata"].(map[string]any)["name"].(string)
			path := r.URL.Path + "/" + name
			if _, ok := f.objs[path]; ok {
				w.WriteHeader(http.StatusConflict)
				_, _ = w.Write([]byte(`{"message":"already exists","reason":"AlreadyExists","code":409}`))
				return
			}
			f.objs[path] = raw
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write(raw)
		case http.MethodPatch, http.MethodDelete:
			if _, ok := f.objs[r.URL.Path]; !ok {
				notFound(w)
				return
			}
			if r.Method == http.MethodDelete {
				delete(f.objs, r.URL.Path)
			}
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeAPI) put(path, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.objs[path] = json.RawMessage(body)
}

func (f *fakeAPI) requests(method string) []request {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []request
	for _, r := range f.seen {
		if r.Method == method {
			out = append(out, r)
		}
	}
	return out
}

// Written out so the tests pin the paths.
const (
	setsPath = "/apis/apps/v1/namespaces/agents/statefulsets/"
	podsPath = "/api/v1/namespaces/agents/pods/"
)

// podOf is the agent's pod, the StatefulSet's first and only.
func podOf(agentID string) string { return podsPath + objectName(agentID) + "-0" }

// driver uses Insecure only so no CA bundle is needed; the fake is plain HTTP.
func (f *fakeAPI) driver(t *testing.T, opts KubernetesOptions) *Kubernetes {
	t.Helper()
	opts.Kube = KubeConfig{Server: f.srv.URL, Token: "test", Insecure: true}
	if opts.Namespace == "" {
		opts.Namespace = "agents"
	}
	opts.Log = slog.New(slog.DiscardHandler)
	k, err := NewKubernetes(context.Background(), opts)
	if err != nil {
		t.Fatalf("building the driver: %v", err)
	}
	return k
}

func testSpec() Spec {
	return Spec{
		AgentID: "agt_06c1k2rt8g3m4n5p6q7r8s9t0v", Name: "Support bot",
		Image: "registry.example/keera-agent:1",
		Class: Class{Isolation: VM, CPU: "2", Memory: "4Gi", Disk: "20Gi"},
		Owner: "ada@example.ch", Org: "org_1", Project: "prj_1",
		Env: map[string]string{"KEERA_AGENTS_RUNTIME_TOKEN": "rt_secret", "AAA": "1"},
	}
}

func TestObjectName(t *testing.T) {
	id := "agt_06c1k2rt8g3m4n5p6q7r8s9t0v"
	if got := objectName(id); got != "keera-agent-agt-06c1k2rt8g3m4n5p6q7r8s9t0v" {
		t.Errorf("objectName = %q", got)
	}
	long := strings.Repeat("A", 80)
	a, b := objectName(long+"1"), objectName(long+"2")
	if len(a) > maxName || a == b {
		t.Errorf("long ids give %q and %q; want distinct names of at most %d", a, b, maxName)
	}
	for _, n := range []string{a, objectName("x_Y.z"), objectName("-odd-")} {
		for _, c := range n {
			if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
				t.Errorf("objectName %q is not a DNS label", n)
			}
		}
		if strings.HasSuffix(n, "-") {
			t.Errorf("objectName %q ends in a dash", n)
		}
	}
}

func TestCreateBuildsTheObject(t *testing.T) {
	f := newFakeAPI(t)
	k := f.driver(t, KubernetesOptions{
		Runtimes:         map[Isolation]string{VM: "kata-qemu"},
		StorageClass:     "fast-local",
		ImagePullSecrets: []string{"regcred"},
	})
	st, err := k.Create(context.Background(), testSpec())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if st.State != Pending {
		t.Errorf("state = %s, want pending", st.State)
	}
	posts := f.requests(http.MethodPost)
	if len(posts) != 1 {
		t.Fatalf("%d creates, want 1", len(posts))
	}
	body := posts[0].Raw
	for _, want := range []string{
		`"kind":"StatefulSet"`,
		`"apiVersion":"apps/v1"`,
		`"name":"keera-agent-agt-06c1k2rt8g3m4n5p6q7r8s9t0v"`,
		`"replicas":1`,
		`"matchLabels":{"app.kubernetes.io/name":"keera-agent","keera.dev/agent-id":"agt_06c1k2rt8g3m4n5p6q7r8s9t0v"}`,
		`"podManagementPolicy":"Parallel"`,
		`"persistentVolumeClaimRetentionPolicy":{"whenDeleted":"Delete","whenScaled":"Retain"}`,
		`"runtimeClassName":"kata-qemu"`,
		`"image":"registry.example/keera-agent:1"`,
		`"cpu":"2"`,
		`"memory":"4Gi"`,
		`"storage":"20Gi"`,
		`"storageClassName":"fast-local"`,
		`"mountPath":"/workspace"`,
		`"imagePullSecrets":[{"name":"regcred"}]`,
		`"restartPolicy":"Always"`,
		// Hardening that applies whatever the isolation tier is.
		`"allowPrivilegeEscalation":false`,
		`"automountServiceAccountToken":false`,
		`"runAsNonRoot":true`,
		`"runAsUser":1000`,
		`"fsGroup":1000`,
		`"seccompProfile":{"type":"RuntimeDefault"}`,
		`"drop":["ALL"]`,
		`"readOnlyRootFilesystem":true`,
		`"enableServiceLinks":false`,
		`"ephemeral-storage":"1Gi"`,
		`{"name":"tmp","mountPath":"/tmp"}`,
		`"volumes":[{"name":"tmp","emptyDir":{"sizeLimit":"1Gi"}}]`,
		// Whose it is.
		`"keera.dev/org":"org_1"`,
		`"keera.dev/project":"prj_1"`,
		`"keera.dev/owner":"ada@example.ch"`,
		`"keera.dev/agent-name":"Support bot"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the created object does not contain %s\n%s", want, body)
		}
	}
	// User namespaces are opt-in.
	if strings.Contains(body, `"hostUsers"`) {
		t.Errorf("hostUsers is set without UserNamespaces:\n%s", body)
	}
}

// User namespaces apply to the standard tier only: the others have a kernel
// boundary of their own, and their runtimes may not support them.
func TestUserNamespacesForTheStandardTier(t *testing.T) {
	for _, tc := range []struct {
		tier Isolation
		want bool
	}{{Standard, true}, {Isolated, false}, {VM, false}} {
		f := newFakeAPI(t)
		k := f.driver(t, KubernetesOptions{
			Runtimes:       map[Isolation]string{Isolated: "gvisor", VM: "kata-qemu"},
			UserNamespaces: true,
		})
		spec := testSpec()
		spec.Class.Isolation = tc.tier
		if _, err := k.Create(context.Background(), spec); err != nil {
			t.Fatalf("%s: create: %v", tc.tier, err)
		}
		posts := f.requests(http.MethodPost)
		if got := strings.Contains(posts[0].Raw, `"hostUsers":false`); got != tc.want {
			t.Errorf("%s: hostUsers false is %v, want %v", tc.tier, got, tc.want)
		}
	}
}

func TestCreateRefusesAnUnmappedTier(t *testing.T) {
	f := newFakeAPI(t)
	k := f.driver(t, KubernetesOptions{})
	_, err := k.Create(context.Background(), testSpec())
	if !errors.Is(err, ErrTier) || !strings.Contains(err.Error(), "KEERA_AGENTS_RUNTIME_VM") {
		t.Fatalf("Create = %v, want ErrTier naming the setting", err)
	}
	if len(f.requests(http.MethodPost)) != 0 {
		t.Error("nothing should have been created")
	}
}

func TestCreateRefusesABadQuantity(t *testing.T) {
	f := newFakeAPI(t)
	k := f.driver(t, KubernetesOptions{Runtimes: map[Isolation]string{VM: "kata"}})
	spec := testSpec()
	spec.Class.Memory = "lots"
	if _, err := k.Create(context.Background(), spec); err == nil {
		t.Fatal("a class with memory \"lots\" was accepted")
	}
}

// Creating again updates the pod template in place, which the StatefulSet
// rolls out, resumes a suspended agent, and leaves the workspace claim where
// it is.
func TestCreateAgainUpdatesTheTemplateAndKeepsTheVolume(t *testing.T) {
	f := newFakeAPI(t)
	k := f.driver(t, KubernetesOptions{Runtimes: map[Isolation]string{VM: "kata"}})
	spec := testSpec()
	if _, err := k.Create(context.Background(), spec); err != nil {
		t.Fatal(err)
	}

	spec.Image = "registry.example/keera-agent:2"
	spec.Class.Isolation = Standard
	if _, err := k.Create(context.Background(), spec); err != nil {
		t.Fatalf("create with a new image: %v", err)
	}

	patches := f.requests(http.MethodPatch)
	if len(patches) != 1 || patches[0].ContentType != "application/json-patch+json" {
		t.Fatalf("patches = %+v, want one JSON patch", patches)
	}
	var ops []jsonPatchOp
	if err := json.Unmarshal([]byte(patches[0].Raw), &ops); err != nil {
		t.Fatal(err)
	}
	var tmpl string
	resumed := false
	for _, op := range ops {
		if op.Path == "/spec/template" && op.Op == "replace" {
			raw, _ := json.Marshal(op.Value)
			tmpl = string(raw)
		}
		if op.Path == "/spec/replicas" && op.Value == float64(1) {
			resumed = true
		}
		if strings.HasPrefix(op.Path, "/spec/volumeClaimTemplates") {
			t.Errorf("volumeClaimTemplates cannot change, but the patch touches them: %+v", op)
		}
	}
	if !strings.Contains(tmpl, "keera-agent:2") {
		t.Errorf("the new pod template lacks the new image: %s", tmpl)
	}
	if !resumed {
		t.Errorf("the patch does not set one replica: %+v", ops)
	}
	// Replaced whole, so the old tier's RuntimeClass does not linger.
	if strings.Contains(tmpl, "runtimeClassName") {
		t.Errorf("the standard tier should have no RuntimeClass: %s", tmpl)
	}

	if deletes := f.requests(http.MethodDelete); len(deletes) != 0 {
		t.Fatalf("deletes = %+v, want none", deletes)
	}
}

func TestStatus(t *testing.T) {
	cases := []struct {
		name   string
		obj    string
		pod    string
		state  State
		detail string
	}{
		{
			"ready",
			`{"metadata":{"generation":2},"spec":{"replicas":1},
			  "status":{"observedGeneration":2,"readyReplicas":1,"updatedReplicas":1}}`,
			"", Running, "",
		},
		{
			"suspended",
			`{"spec":{"replicas":0},"status":{"observedGeneration":3}}`,
			"", Suspended, "suspended; its volume is kept",
		},
		{
			"a change not seen yet",
			`{"metadata":{"generation":3},"spec":{"replicas":1},
			  "status":{"observedGeneration":2,"readyReplicas":1,"updatedReplicas":1}}`,
			`{"status":{"phase":"Running"}}`, Pending, "starting",
		},
		{
			"unschedulable",
			`{"metadata":{"generation":1},"spec":{"replicas":1},"status":{"observedGeneration":1}}`,
			`{"status":{"phase":"Pending","conditions":[
			  {"type":"PodScheduled","status":"False","reason":"Unschedulable",
			   "message":"0/6 nodes are available: 6 Insufficient memory."}]}}`,
			Pending, "0/6 nodes are available: 6 Insufficient memory.",
		},
		{
			"crash looping",
			`{"metadata":{"generation":1},"spec":{"replicas":1},"status":{"observedGeneration":1}}`,
			`{"status":{"phase":"Running","containerStatuses":[{"state":{"waiting":
			  {"reason":"CrashLoopBackOff","message":"back-off 5m0s"}}}]}}`,
			Failed, "CrashLoopBackOff: back-off 5m0s",
		},
		{
			"no pod yet",
			`{"spec":{"replicas":1},"status":{}}`,
			"", Pending, "waiting for its pod; if it stays so, see the StatefulSet's events",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeAPI(t)
			k := f.driver(t, KubernetesOptions{})
			f.put(setsPath+objectName("agt_1"), tc.obj)
			if tc.pod != "" {
				f.put(podOf("agt_1"), tc.pod)
			}
			st, err := k.Status(context.Background(), "agt_1")
			if err != nil {
				t.Fatalf("status: %v", err)
			}
			if st.State != tc.state || (tc.detail != "" && st.Detail != tc.detail) {
				t.Errorf("status = %s %q, want %s %q", st.State, st.Detail, tc.state, tc.detail)
			}
		})
	}
}

// The fleet compares a running pod's token with the stored one.
func TestStatusHasTheToken(t *testing.T) {
	f := newFakeAPI(t)
	k := f.driver(t, KubernetesOptions{})
	f.put(setsPath+objectName("agt_1"), `{"metadata":{"generation":1},
	  "spec":{"replicas":1,"template":{"spec":{"containers":[{"name":"agent",
	    "env":[{"name":"PATH","value":"/bin"},{"name":"KEERA_AGENTS_RUNTIME_TOKEN","value":"kat_1"}]}]}}},
	  "status":{"observedGeneration":1,"readyReplicas":1,"updatedReplicas":1}}`)
	st, err := k.Status(context.Background(), "agt_1")
	if err != nil || st.State != Running || st.Token != "kat_1" {
		t.Errorf("Status = %+v, %v; want running with kat_1", st, err)
	}
}

func TestStatusOfAMissingAgentIsGone(t *testing.T) {
	f := newFakeAPI(t)
	k := f.driver(t, KubernetesOptions{})
	st, err := k.Status(context.Background(), "agt_gone")
	if err != nil || st.State != Gone {
		t.Fatalf("Status = %+v, %v; want gone", st, err)
	}
}

func TestTerminateIsIdempotent(t *testing.T) {
	f := newFakeAPI(t)
	k := f.driver(t, KubernetesOptions{})
	f.put(setsPath+objectName("agt_1"), `{}`)
	for range 2 {
		if err := k.Terminate(context.Background(), "agt_1"); err != nil {
			t.Fatalf("terminate: %v", err)
		}
	}
}

func TestSuspendResumeScale(t *testing.T) {
	f := newFakeAPI(t)
	k := f.driver(t, KubernetesOptions{})
	f.put(setsPath+objectName("agt_1"), `{"spec":{"replicas":1}}`)
	if err := k.Suspend(context.Background(), "agt_1"); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	if err := k.Resume(context.Background(), "agt_1"); err != nil {
		t.Fatalf("resume: %v", err)
	}
	var replicas []float64
	for _, r := range f.requests(http.MethodPatch) {
		spec, _ := r.Body["spec"].(map[string]any)
		if n, ok := spec["replicas"].(float64); ok {
			replicas = append(replicas, n)
		}
	}
	if len(replicas) != 2 || replicas[0] != 0 || replicas[1] != 1 {
		t.Errorf("patched replicas = %v, want [0 1]", replicas)
	}
	if err := k.Suspend(context.Background(), "agt_gone"); !errors.Is(err, ErrNotFound) {
		t.Errorf("suspending a missing agent = %v, want ErrNotFound", err)
	}
}

func TestLogsReadThePod(t *testing.T) {
	f := newFakeAPI(t)
	k := f.driver(t, KubernetesOptions{})
	if _, err := k.Logs(context.Background(), "agt_1", false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Logs without a pod = %v, want ErrNotFound", err)
	}
	f.put(podOf("agt_1"), `{}`)
	rc, err := k.Logs(context.Background(), "agt_1", true)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	out, _ := io.ReadAll(rc)
	if string(out) != "hello from the runtime\n" {
		t.Errorf("logs = %q", out)
	}
	gets := f.requests(http.MethodGet)
	if q := gets[len(gets)-1].Query; q != "container=agent&follow=true" {
		t.Errorf("query = %q", q)
	}
}

func TestTiersFollowTheRuntimes(t *testing.T) {
	f := newFakeAPI(t)
	k := f.driver(t, KubernetesOptions{Runtimes: map[Isolation]string{VM: "kata"}})
	if got := k.Tiers(); len(got) != 2 || got[0] != Standard || got[1] != VM {
		t.Errorf("Tiers = %v, want [standard vm]", got)
	}
}

func TestParseQuantity(t *testing.T) {
	for in, want := range map[string]float64{
		"2": 2, "500m": 0.5, "4Gi": 4 << 30, "512Mi": 512 << 20, "1.5G": 1.5e9, "100k": 1e5,
	} {
		if got, err := parseQuantity(in); err != nil || got != want {
			t.Errorf("parseQuantity(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "Gi", "lots", "-1", "4GB"} {
		if _, err := parseQuantity(bad); err == nil {
			t.Errorf("parseQuantity(%q) succeeded", bad)
		}
	}
}

func TestAWorkspaceDeclaresItsFrontAndIsLabelled(t *testing.T) {
	f := newFakeAPI(t)
	k := f.driver(t, KubernetesOptions{Runtimes: map[Isolation]string{VM: "kata"}})
	agent := testSpec()
	if _, err := k.Create(context.Background(), agent); err != nil {
		t.Fatal(err)
	}
	ws := testSpec()
	ws.AgentID, ws.Workspace = "wks_1", true
	if _, err := k.Create(context.Background(), ws); err != nil {
		t.Fatal(err)
	}
	posts := f.requests(http.MethodPost)
	if a := posts[0].Raw; strings.Contains(a, `"containerPort"`) || strings.Contains(a, `"app.kubernetes.io/component"`) {
		t.Errorf("an agent takes no connections, so it has no port nor the workspace label:\n%s", a)
	}
	for _, want := range []string{
		`"ports":[{"name":"front","containerPort":7420,"protocol":"TCP"}]`,
		`"app.kubernetes.io/component":"workspace"`,
	} {
		if !strings.Contains(posts[1].Raw, want) {
			t.Errorf("the workspace lacks %s\n%s", want, posts[1].Raw)
		}
	}
}

func TestAddressIsThePodsIP(t *testing.T) {
	f := newFakeAPI(t)
	k := f.driver(t, KubernetesOptions{})
	ctx := context.Background()
	if _, err := k.Address(ctx, "wks_1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Address without a pod = %v, want ErrNotFound", err)
	}
	f.put(podOf("wks_1"), `{"status":{"phase":"Pending"}}`)
	if _, err := k.Address(ctx, "wks_1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Address of a pending pod = %v, want ErrNotFound", err)
	}
	f.put(podOf("wks_1"), `{"status":{"phase":"Running","podIP":"10.1.2.3"}}`)
	addr, err := k.Address(ctx, "wks_1")
	if err != nil || addr != "10.1.2.3:7420" {
		t.Fatalf("Address = %q, %v", addr, err)
	}
}
