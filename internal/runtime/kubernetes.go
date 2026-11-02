package runtime

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/bespinian/keera-agents/internal/protocol"
)

// The Kubernetes driver makes one StatefulSet per agent, with a single pod and
// a workspace claim, so it runs on any cluster as it is, with no controller of
// its own:
//
//	spec.replicas             1 while it runs, 0 while suspended
//	spec.template             the runtime's pod
//	volumeClaimTemplates      the workspace, kept while suspended
//	persistentVolumeClaimRetentionPolicy
//	                          deletes the workspace with the agent
//
// The driver talks plain HTTPS to the API server instead of using client-go,
// which would grow the module graph a lot to save a few hundred lines of JSON.

const setsAPIPath = "/apis/apps/v1"

type KubernetesOptions struct {
	Kube KubeConfig
	// Namespace should be the agents' own: anybody who can read pods there
	// can read every agent's runtime token.
	Namespace string
	// Runtimes maps isolation tiers to RuntimeClass names. A tier other than
	// standard with no entry is refused, never downgraded.
	Runtimes map[Isolation]string
	// StorageClass empty uses the cluster default.
	StorageClass string
	// ServiceAccount empty is the namespace's default. No token is mounted
	// either way.
	ServiceAccount   string
	ImagePullSecrets []string
	// UserNamespaces runs standard-tier pods with hostUsers: false, so the
	// agent's user maps to an unprivileged one on the node. It needs
	// Kubernetes 1.33 or later and a container runtime that supports it. The
	// other tiers have a kernel boundary of their own, and their runtimes may
	// not support it.
	UserNamespaces bool
	Log            *slog.Logger
}

type Kubernetes struct {
	c    *kubeClient
	opts KubernetesOptions
}

// NewKubernetes lists StatefulSets as a check, which proves the service
// account may use them.
func NewKubernetes(ctx context.Context, opts KubernetesOptions) (*Kubernetes, error) {
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	if opts.Namespace == "" {
		// Allowed so an install without it still starts, but warned about.
		opts.Namespace = inClusterNamespace()
		opts.Log.Warn("KEERA_AGENTS_NAMESPACE is not set, so agents run in keera-agents' own "+
			"namespace, where anyone who can read pods can read their runtime tokens; "+
			"give them a namespace of their own", "namespace", opts.Namespace)
	}
	c, err := newKubeClient(opts.Kube)
	if err != nil {
		return nil, err
	}
	k := &Kubernetes{c: c, opts: opts}

	var probe struct {
		Items []struct{} `json:"items"`
	}
	if err := c.get(ctx, k.collection()+"?limit=1", &probe); err != nil {
		return nil, fmt.Errorf("listing StatefulSets in namespace %s: %w; keera-agents needs "+
			"the Role in docs/agents.md there", opts.Namespace, err)
	}
	return k, nil
}

func (k *Kubernetes) Name() string { return "kubernetes" }

func (k *Kubernetes) Tiers() []Isolation { return tiersFor(k.opts.Runtimes) }

func (k *Kubernetes) collection() string {
	return setsAPIPath + "/namespaces/" + url.PathEscape(k.opts.Namespace) + "/statefulsets"
}

func (k *Kubernetes) object(agentID string) string {
	return k.collection() + "/" + url.PathEscape(objectName(agentID))
}

// podPath is the agent's pod: a StatefulSet names its pods after itself and
// their ordinal.
func (k *Kubernetes) podPath(agentID string) string {
	return "/api/v1/namespaces/" + url.PathEscape(k.opts.Namespace) + "/pods/" +
		url.PathEscape(objectName(agentID)+"-0")
}

// kubeStatefulSet is the part of a StatefulSet this driver uses.
type kubeStatefulSet struct {
	APIVersion string        `json:"apiVersion,omitempty"`
	Kind       string        `json:"kind,omitempty"`
	Metadata   kubeMeta      `json:"metadata"`
	Spec       kubeSetSpec   `json:"spec"`
	Status     kubeSetStatus `json:"status"`
}

type kubeMeta struct {
	Name        string            `json:"name,omitempty"`
	Namespace   string            `json:"namespace,omitempty"`
	Generation  int64             `json:"generation,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

type kubeSetSpec struct {
	Replicas *int32 `json:"replicas"`
	// ServiceName names no Service: nothing calls an agent. Older clusters
	// require the field.
	ServiceName         string           `json:"serviceName"`
	Selector            kubeSelector     `json:"selector"`
	PodManagementPolicy string           `json:"podManagementPolicy,omitempty"`
	Template            kubePodTmpl      `json:"template"`
	VolumeClaims        []kubePVCTmpl    `json:"volumeClaimTemplates,omitempty"`
	ClaimRetention      *kubeClaimRetain `json:"persistentVolumeClaimRetentionPolicy,omitempty"`
}

type kubeSelector struct {
	MatchLabels map[string]string `json:"matchLabels"`
}

type kubeClaimRetain struct {
	WhenDeleted string `json:"whenDeleted"`
	WhenScaled  string `json:"whenScaled"`
}

type kubeSetStatus struct {
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	ReadyReplicas      int32 `json:"readyReplicas,omitempty"`
	UpdatedReplicas    int32 `json:"updatedReplicas,omitempty"`
}

type kubePodTmpl struct {
	Metadata kubeMeta `json:"metadata"`
	Spec     kubePod  `json:"spec"`
}

type kubePod struct {
	RuntimeClassName             *string          `json:"runtimeClassName,omitempty"`
	ServiceAccountName           string           `json:"serviceAccountName,omitempty"`
	AutomountServiceAccountToken *bool            `json:"automountServiceAccountToken"`
	EnableServiceLinks           *bool            `json:"enableServiceLinks"`
	HostUsers                    *bool            `json:"hostUsers,omitempty"`
	SecurityContext              *kubePodSecurity `json:"securityContext,omitempty"`
	Containers                   []kubeContainer  `json:"containers"`
	Volumes                      []kubeVolume     `json:"volumes,omitempty"`
	ImagePullSecrets             []kubeNameRef    `json:"imagePullSecrets,omitempty"`
	RestartPolicy                string           `json:"restartPolicy,omitempty"`
}

type kubeVolume struct {
	Name     string        `json:"name"`
	EmptyDir *kubeEmptyDir `json:"emptyDir,omitempty"`
}

type kubeEmptyDir struct {
	SizeLimit string `json:"sizeLimit,omitempty"`
}

type kubePodSecurity struct {
	RunAsNonRoot   *bool        `json:"runAsNonRoot,omitempty"`
	RunAsUser      *int64       `json:"runAsUser,omitempty"`
	RunAsGroup     *int64       `json:"runAsGroup,omitempty"`
	FSGroup        *int64       `json:"fsGroup,omitempty"`
	SeccompProfile *kubeSeccomp `json:"seccompProfile,omitempty"`
}

type kubeSeccomp struct {
	Type string `json:"type"`
}

type kubeContainer struct {
	Name            string           `json:"name"`
	Image           string           `json:"image"`
	Env             []kubeEnv        `json:"env,omitempty"`
	Resources       kubeResources    `json:"resources"`
	VolumeMounts    []kubeMount      `json:"volumeMounts,omitempty"`
	SecurityContext *kubeCtrSecurity `json:"securityContext,omitempty"`
}

type kubeCtrSecurity struct {
	AllowPrivilegeEscalation *bool     `json:"allowPrivilegeEscalation"`
	ReadOnlyRootFilesystem   *bool     `json:"readOnlyRootFilesystem"`
	Capabilities             *kubeCaps `json:"capabilities,omitempty"`
}

type kubeCaps struct {
	Drop []string `json:"drop,omitempty"`
}

type kubeEnv struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type kubeResources struct {
	Requests map[string]string `json:"requests,omitempty"`
	Limits   map[string]string `json:"limits,omitempty"`
}

type kubeMount struct {
	Name      string `json:"name"`
	MountPath string `json:"mountPath"`
}

type kubeNameRef struct {
	Name string `json:"name"`
}

type kubePVCTmpl struct {
	Metadata kubeMeta    `json:"metadata"`
	Spec     kubePVCSpec `json:"spec"`
}

type kubePVCSpec struct {
	AccessModes      []string      `json:"accessModes"`
	StorageClassName *string       `json:"storageClassName,omitempty"`
	Resources        kubeResources `json:"resources"`
}

type kubeCondition struct {
	Type    string `json:"type"`
	Status  string `json:"status"`
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
}

func conditionOf(conds []kubeCondition, typ string) (kubeCondition, bool) {
	for _, c := range conds {
		if c.Type == typ {
			return c, true
		}
	}
	return kubeCondition{}, false
}

// The volumes and the one container of an agent's pod.
const (
	workspaceVolume  = "workspace"
	tmpVolume        = "tmp"
	podContainerName = "agent"
)

// tmpSize caps /tmp, and with it all the node disk an agent's pod may use
// besides its workspace: the root filesystem is read-only, and the logs are
// small.
const tmpSize = "1Gi"

// Create writes the StatefulSet, or updates the one that is there.
//
// An existing agent is updated in place, never deleted and created again, so
// its workspace claim stays. The StatefulSet rolls its pod when the template
// changes. volumeClaimTemplates cannot change, so a larger Disk only applies
// to a new agent.
//
// The spec's hash is kept in an annotation, so a Create with an unchanged
// spec, such as a retry, writes nothing.
func (k *Kubernetes) Create(ctx context.Context, spec Spec) (Status, error) {
	if err := checkSpec(spec); err != nil {
		return Status{}, err
	}
	rc, err := runtimeFor(spec.Class, k.opts.Runtimes, k.Name())
	if err != nil {
		return Status{}, err
	}
	obj := k.build(spec, rc)

	err = k.c.post(ctx, k.collection(), obj, nil)
	if err == nil {
		return Status{State: Pending, Detail: "created; waiting for its pod"}, nil
	}
	if !isConflict(err) {
		return Status{}, err
	}

	var existing kubeStatefulSet
	if err := k.c.get(ctx, k.object(spec.AgentID), &existing); err != nil {
		return Status{}, err
	}
	hash := obj.Metadata.Annotations[annotationSpecHash]
	if existing.Metadata.Annotations[annotationSpecHash] == hash {
		if r := existing.Spec.Replicas; r == nil || *r != 1 {
			if err := k.setReplicas(ctx, spec.AgentID, 1); err != nil {
				return Status{}, err
			}
		}
		return k.Status(ctx, spec.AgentID)
	}

	ops := []jsonPatchOp{
		{Op: "replace", Path: "/spec/template", Value: obj.Spec.Template},
		{Op: "add", Path: "/spec/replicas", Value: 1},
	}
	// Key by key, so labels and annotations others put on it stay.
	for _, kv := range []struct {
		field string
		m     map[string]string
		had   map[string]string
	}{
		{"labels", obj.Metadata.Labels, existing.Metadata.Labels},
		{"annotations", obj.Metadata.Annotations, existing.Metadata.Annotations},
	} {
		if kv.had == nil {
			ops = append(ops, jsonPatchOp{Op: "add", Path: "/metadata/" + kv.field, Value: kv.m})
			continue
		}
		for _, name := range slices.Sorted(maps.Keys(kv.m)) {
			ops = append(ops, jsonPatchOp{
				Op: "add", Path: "/metadata/" + kv.field + "/" + jsonPointerEscape(name), Value: kv.m[name],
			})
		}
	}
	if err := k.c.jsonPatch(ctx, k.object(spec.AgentID), ops); err != nil {
		return Status{}, kubeNotFound(err)
	}
	return k.Status(ctx, spec.AgentID)
}

func jsonPointerEscape(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "~", "~0"), "/", "~1")
}

func (k *Kubernetes) build(spec Spec, runtimeClass string) *kubeStatefulSet {
	labels := labelsFor(spec)
	name := objectName(spec.AgentID)

	pod := kubePod{
		ServiceAccountName: k.opts.ServiceAccount,
		// The runtime runs whatever a model asks for and has no business with
		// the API server.
		AutomountServiceAccountToken: new(false),
		// Nor with the namespace's other Services in its environment.
		EnableServiceLinks: new(false),
		SecurityContext: &kubePodSecurity{
			RunAsNonRoot:   new(true),
			RunAsUser:      new(int64(agentUID)),
			RunAsGroup:     new(int64(agentUID)),
			FSGroup:        new(int64(agentUID)),
			SeccompProfile: &kubeSeccomp{Type: "RuntimeDefault"},
		},
		Containers: []kubeContainer{agentContainer(spec)},
		Volumes:    []kubeVolume{{Name: tmpVolume, EmptyDir: &kubeEmptyDir{SizeLimit: tmpSize}}},
		// The runtime is long-lived: a crash restarts it on the same volume.
		RestartPolicy: "Always",
	}
	if runtimeClass != "" {
		pod.RuntimeClassName = &runtimeClass
	}
	if k.opts.UserNamespaces && spec.Class.Isolation == Standard {
		pod.HostUsers = new(false)
	}
	for _, name := range k.opts.ImagePullSecrets {
		pod.ImagePullSecrets = append(pod.ImagePullSecrets, kubeNameRef{Name: name})
	}

	obj := &kubeStatefulSet{
		APIVersion: "apps/v1",
		Kind:       "StatefulSet",
		Metadata: kubeMeta{
			Name:        name,
			Namespace:   k.opts.Namespace,
			Labels:      labels,
			Annotations: annotationsFor(spec),
		},
		Spec: kubeSetSpec{
			Replicas:    new(int32(1)),
			ServiceName: name,
			// The selector cannot change, so it holds only what never does.
			Selector: kubeSelector{MatchLabels: map[string]string{
				labelName:    labels[labelName],
				labelAgentID: labels[labelAgentID],
			}},
			// The default, OrderedReady, stops a rollout while the pod is not
			// ready, so a crash-looping agent would never get a fixed image.
			PodManagementPolicy: "Parallel",
			Template:            kubePodTmpl{Metadata: kubeMeta{Labels: labels}, Spec: pod},
			VolumeClaims: []kubePVCTmpl{{
				Metadata: kubeMeta{Name: workspaceVolume, Labels: labels},
				Spec: kubePVCSpec{
					AccessModes:      []string{"ReadWriteOnce"},
					StorageClassName: nullableString(k.opts.StorageClass),
					Resources:        kubeResources{Requests: map[string]string{"storage": spec.Class.Disk}},
				},
			}},
			// Suspending keeps the workspace; terminating deletes it.
			ClaimRetention: &kubeClaimRetain{WhenDeleted: "Delete", WhenScaled: "Retain"},
		},
	}
	// The pod template alone: the volume claims cannot change anyway.
	raw, _ := json.Marshal(obj.Spec.Template)
	obj.Metadata.Annotations[annotationSpecHash] = hashOf(raw)
	return obj
}

func agentContainer(spec Spec) kubeContainer {
	res := map[string]string{
		"cpu":               spec.Class.CPU,
		"memory":            spec.Class.Memory,
		"ephemeral-storage": tmpSize,
	}
	return kubeContainer{
		Name:  podContainerName,
		Image: spec.Image,
		Env:   envList(spec.Env),
		// Requests equal limits, so a noisy neighbour cannot slow it.
		Resources: kubeResources{Requests: res, Limits: maps.Clone(res)},
		VolumeMounts: []kubeMount{
			{Name: workspaceVolume, MountPath: protocol.Workspace},
			{Name: tmpVolume, MountPath: "/tmp"},
		},
		SecurityContext: &kubeCtrSecurity{
			AllowPrivilegeEscalation: new(false),
			// The agent runs as a user who can write nowhere else anyway. Read
			// only, its writes cannot fill the node's disk, and a derived
			// image's files stay as built. Tools go in the workspace.
			ReadOnlyRootFilesystem: new(true),
			Capabilities:           &kubeCaps{Drop: []string{"ALL"}},
		},
	}
}

func (k *Kubernetes) Status(ctx context.Context, agentID string) (Status, error) {
	var obj kubeStatefulSet
	if err := k.c.get(ctx, k.object(agentID), &obj); err != nil {
		if isNotFound(err) {
			return Status{State: Gone, Detail: "there is no StatefulSet"}, nil
		}
		return Status{}, err
	}
	if r := obj.Spec.Replicas; r != nil && *r == 0 {
		return Status{State: Suspended, Detail: "suspended; its volume is kept"}, nil
	}
	// After a change, the old pod may still be ready until the controller has
	// seen the new spec and replaced it.
	st := obj.Status
	if st.ObservedGeneration >= obj.Metadata.Generation && st.ReadyReplicas > 0 && st.UpdatedReplicas > 0 {
		// The pod is the template's, so it has the template's token.
		var token string
		for _, c := range obj.Spec.Template.Spec.Containers {
			if c.Name != podContainerName {
				continue
			}
			for _, e := range c.Env {
				if e.Name == protocol.EnvRuntimeToken {
					token = e.Value
				}
			}
		}
		return Status{State: Running, Token: token}, nil
	}
	return k.podStatus(ctx, agentID), nil
}

// failingReasons are a waiting container's reasons that will not go away by
// themselves.
var failingReasons = []string{
	"CrashLoopBackOff", "ErrImagePull", "ImagePullBackOff", "InvalidImageName",
	"CreateContainerConfigError", "CreateContainerError", "RunContainerError",
}

// podStatus says why an agent that should run is not ready, from its pod. It
// needs get on pods. Without it, or without a pod, the agent stays pending.
func (k *Kubernetes) podStatus(ctx context.Context, agentID string) Status {
	var pod struct {
		Status struct {
			Phase             string          `json:"phase"`
			Message           string          `json:"message"`
			Conditions        []kubeCondition `json:"conditions"`
			ContainerStatuses []struct {
				State struct {
					Waiting *struct {
						Reason  string `json:"reason"`
						Message string `json:"message"`
					} `json:"waiting"`
				} `json:"state"`
			} `json:"containerStatuses"`
		} `json:"status"`
	}
	if err := k.c.get(ctx, k.podPath(agentID), &pod); err != nil {
		// A quota or pod security admission refusing the pod shows only in
		// the StatefulSet's events.
		return Status{State: Pending, Detail: "waiting for its pod; if it stays so, see the StatefulSet's events"}
	}
	if pod.Status.Phase == "Failed" {
		return Status{State: Failed, Detail: cmp.Or(pod.Status.Message, "the pod failed")}
	}
	for _, cs := range pod.Status.ContainerStatuses {
		if w := cs.State.Waiting; w != nil && slices.Contains(failingReasons, w.Reason) {
			if w.Message != "" {
				return Status{State: Failed, Detail: w.Reason + ": " + w.Message}
			}
			return Status{State: Failed, Detail: w.Reason}
		}
	}
	if c, ok := conditionOf(pod.Status.Conditions, "PodScheduled"); ok && c.Status == "False" {
		// The scheduler's message says best why it is still starting.
		return Status{State: Pending, Detail: cmp.Or(c.Message, c.Reason, "waiting to be scheduled")}
	}
	return Status{State: Pending, Detail: "starting"}
}

// Suspend, like Resume, patches one field, so two control planes doing it
// agree.
func (k *Kubernetes) Suspend(ctx context.Context, agentID string) error {
	return k.setReplicas(ctx, agentID, 0)
}

func (k *Kubernetes) Resume(ctx context.Context, agentID string) error {
	return k.setReplicas(ctx, agentID, 1)
}

func (k *Kubernetes) setReplicas(ctx context.Context, agentID string, n int32) error {
	patch := map[string]any{"spec": map[string]any{"replicas": n}}
	return kubeNotFound(k.c.patch(ctx, k.object(agentID), patch))
}

// Terminate removes the StatefulSet, its pod and, through the claim retention
// policy, its workspace. One already gone is success.
func (k *Kubernetes) Terminate(ctx context.Context, agentID string) error {
	if err := k.c.delete(ctx, k.object(agentID)); err != nil && !isNotFound(err) {
		return err
	}
	return nil
}

// Logs reads the pod's log, which needs get on pods/log. A suspended agent
// has no pod, which is ErrNotFound. A container that has not started yet
// answers 400.
func (k *Kubernetes) Logs(ctx context.Context, agentID string, follow bool) (io.ReadCloser, error) {
	path := k.podPath(agentID) + "/log?container=" + podContainerName
	if follow {
		path += "&follow=true"
	}
	rc, err := k.c.stream(ctx, path)
	if hasKubeStatus(err, http.StatusBadRequest) {
		return nil, fmt.Errorf("the agent's runtime has not started yet: %w", err)
	}
	return rc, kubeNotFound(err)
}

func envList(env map[string]string) []kubeEnv {
	if len(env) == 0 {
		return nil
	}
	out := make([]kubeEnv, 0, len(env))
	// Sorted, so the same spec gives the same pod template and hash.
	for _, n := range slices.Sorted(maps.Keys(env)) {
		out = append(out, kubeEnv{Name: n, Value: env[n]})
	}
	return out
}

func nullableString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
